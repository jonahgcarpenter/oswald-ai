// Package cron owns per-profile scheduled jobs: validation, file-backed
// storage, schedule parsing, tick dispatch, and an execution ledger.
//
// Storage is filesystem JSON under <profile>/cron/ (0600 files, 0700
// dirs), excluded from durable backups like other cache state. All
// mutations hold an flock on jobs.lock so model-triggered edits and the
// background ticker serialize across instances.
package cron

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AllowedToolsets bounds enabled_toolsets entries.
var AllowedToolsets = []string{"core", "terminal", "code_execution", "cronjob"}

// Schedule kinds for the stored parsed schedule.
const (
	ScheduleCron     = "cron"
	ScheduleInterval = "interval"
	ScheduleOnce     = "once"
)

// Delivery platforms for the concrete deliver target.
const (
	DeliverDiscord  = "discord"
	DeliverIMessage = "imessage"
)

// Job states for the scheduling state machine.
const (
	StateScheduled = "scheduled"
	StatePaused    = "paused"
	StateCompleted = "completed"
	StateRunning   = "running"
)

// FailureStreakLimit auto-pauses a job: this many consecutive failed runs
// park the schedule and alert the deliver target.
const FailureStreakLimit = 3

// RepeatState counts bounded runs: Times == nil repeats forever, which is
// the default for recurring schedules. One-shots default to Times == 1.
type RepeatState struct {
	Times     *int `json:"times"`
	Completed int  `json:"completed"`
}

// Schedule is the stored parsed schedule. All wall-clock occurrences are
// UTC: the model converts local intent before scheduling.
type Schedule struct {
	Kind    string `json:"kind"`
	Expr    string `json:"expr,omitempty"`
	Minutes int    `json:"minutes,omitempty"`
	RunAt   string `json:"run_at,omitempty"`
	Display string `json:"display,omitempty"`
}

// IsRepeat reports whether the schedule recurs.
func (s Schedule) IsRepeat() bool { return s.Kind == ScheduleCron || s.Kind == ScheduleInterval }

// Deliver is the concrete delivery target. There is no origin indirection:
// the target is captured at create time.
type Deliver struct {
	Platform string `json:"platform"`
	ChatID   string `json:"chat_id"`
	UserID   string `json:"user_id,omitempty"`
}

// SessionKey renders the deliver target as a transport session key
// (<platform>:<chat>:<user>) shared with the gateway senders.
func (d Deliver) SessionKey() string {
	return strings.TrimSpace(d.Platform) + ":" + strings.TrimSpace(d.ChatID) + ":" + strings.TrimSpace(d.UserID)
}

// Job is one scheduled unit of work. Timestamps are UTC RFC3339; a null
// next_run_at means unscheduled (paused, completed, or settled one-shot).
type Job struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"`
	Prompt          string      `json:"prompt,omitempty"`
	Skills          []string    `json:"skills,omitempty"`
	Script          string      `json:"script,omitempty"`
	NoAgent         bool        `json:"no_agent"`
	Schedule        Schedule    `json:"schedule"`
	Repeat          RepeatState `json:"repeat"`
	Deliver         Deliver     `json:"deliver"`
	EnabledToolsets []string    `json:"enabled_toolsets,omitempty"`
	Enabled         bool        `json:"enabled"`
	State           string      `json:"state,omitempty"`
	PausedAt        *string     `json:"paused_at"`
	PausedReason    string      `json:"paused_reason,omitempty"`
	CreatedAt       string      `json:"created_at"`
	NextRunAt       *string     `json:"next_run_at"`
	LastRunAt       *string     `json:"last_run_at"`
	LastStatus      string      `json:"last_status,omitempty"`
	LastError       string      `json:"last_delivery_error,omitempty"`
	FailureStreak   int         `json:"failure_streak"`
	// SlotUnix is the claimed occurrence instant for ledger dedupe. It is
	// never persisted; ClaimDue stamps it on the dispatched copy.
	SlotUnix int64 `json:"-"`
}

// EffectivePaused reports whether a job must never fire.
func (j *Job) EffectivePaused() bool {
	return j.State == StatePaused || j.State == StateCompleted
}

// NextRunTime parses the stored next_run_at instant.
func (j *Job) NextRunTime() (time.Time, bool) {
	if j == nil || j.NextRunAt == nil || strings.TrimSpace(*j.NextRunAt) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*j.NextRunAt))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// utcISO formats an instant as explicit UTC RFC3339.
func utcISO(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// normalizePlatform maps operator spellings to the stored platform.
func normalizePlatform(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "discord":
		return DeliverDiscord, nil
	case "imessage", "bluebubbles", "blue_bubbles":
		return DeliverIMessage, nil
	default:
		return "", errors.New("deliver platform must be discord or imessage")
	}
}

// ValidateDeliver checks a concrete delivery target.
func ValidateDeliver(d Deliver) error {
	if _, err := normalizePlatform(d.Platform); err != nil {
		return err
	}
	if strings.TrimSpace(d.ChatID) == "" {
		return errors.New("deliver chat_id is required")
	}
	return nil
}

// Execution is one ledger entry.
type Execution struct {
	JobID             string   `json:"job_id"`
	Instant           int64    `json:"scheduled_instant"`
	StartedUnix       int64    `json:"started_unix"`
	FinishedUnix      int64    `json:"finished_unix"`
	Status            string   `json:"status"`
	OutputPreview     string   `json:"output_preview,omitempty"`
	Delivered         bool     `json:"delivered"`
	DeliveryError     string   `json:"delivery_error,omitempty"`
	RegisteredSenders []string `json:"registered_senders,omitempty"`
}

// Store owns jobs.json for one profile root.
type Store struct {
	root     string
	mu       sync.Mutex
	inFlight map[string]bool
}

// NewStore binds a store to a profile root.
func NewStore(profileRoot string) *Store {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	return &Store{root: abs, inFlight: make(map[string]bool)}
}

// Root returns the bound profile root.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// TryClaim takes the in-flight slot for a job. False means a tick or a
// manual run already owns it.
func (s *Store) TryClaim(id string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight[id] {
		return false
	}
	s.inFlight[id] = true
	return true
}

// Release frees the in-flight slot for a job.
func (s *Store) Release(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, id)
}

func (s *Store) dir() (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("cron store is unavailable")
	}
	return filepath.Join(s.root, "cron"), nil
}

func (s *Store) loadJobs() ([]Job, error) {
	dir, err := s.dir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	// Strict shape with no migration: the operator updates the file by
	// hand, so any legacy or foreign entry fails closed with guidance.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("jobs.json needs manual update (unsupported shape): %v", err)
	}
	jobs := make([]Job, 0, len(raw))
	for i, item := range raw {
		var job Job
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&job); err != nil {
			return nil, fmt.Errorf("jobs.json needs manual update (entry %d unsupported): %v", i, err)
		}
		if strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.Schedule.Kind) == "" {
			return nil, fmt.Errorf("jobs.json needs manual update (entry %d unsupported): missing id or schedule", i)
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Store) saveJobs(jobs []Job) error {
	dir, err := s.dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".jobs-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, "jobs.json")); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "jobs.json"), 0600); err != nil {
		return err
	}
	return nil
}

// List returns jobs newest-first.
func (s *Store) List() ([]Job, error) {
	jobs, err := s.loadJobs()
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedAt > jobs[j].CreatedAt })
	return jobs, nil
}

func newJobID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

var injectionPhrases = []string{
	"ignore previous instructions",
	"disregard system",
	"disregard all prior",
	"jailbreak",
	"dan mode",
}

// ScanPrompt rejects likely prompt-injection content.
func ScanPrompt(prompt string) error {
	lower := strings.ToLower(prompt)
	for _, phrase := range injectionPhrases {
		if strings.Contains(lower, phrase) {
			return errors.New("prompt failed injection scan")
		}
	}
	return nil
}

// ParseSchedule resolves a human or cron schedule into the stored dict
// plus its next UTC occurrence. There is no migration: unparsable input is
// a plain error and unparsable stored jobs record blocked_config.
func ParseSchedule(raw string, now time.Time) (Schedule, time.Time, error) {
	now = now.UTC()
	s := strings.TrimSpace(raw)
	if s == "" {
		return Schedule{}, time.Time{}, errors.New("schedule is required")
	}
	lower := strings.ToLower(s)
	if rest, ok := strings.CutPrefix(lower, "in "); ok {
		d, err := parseDurationWord(strings.TrimSpace(rest))
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
		next := now.Add(d)
		dur := strings.TrimSpace(raw[3:])
		return Schedule{Kind: ScheduleOnce, RunAt: utcISO(next), Display: "once in " + dur}, next, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		next := t.UTC()
		if !next.After(now) {
			return Schedule{}, time.Time{}, errors.New("one-shot schedule must be in the future")
		}
		return Schedule{Kind: ScheduleOnce, RunAt: utcISO(next), Display: "once at " + next.Format("2006-01-02 15:04")}, next, nil
	}
	if rest, ok := strings.CutPrefix(lower, "every "); ok {
		return parseEverySchedule(strings.TrimSpace(rest), strings.TrimSpace(raw), now)
	}
	if d, err := parseDurationWord(lower); err == nil {
		next := now.Add(d)
		mins := durationMinutes(d)
		return Schedule{Kind: ScheduleInterval, Minutes: mins, Display: "every " + s}, next, nil
	}
	if fields := strings.Fields(s); len(fields) == 5 {
		next, err := nextCron(fields, now)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
		return Schedule{Kind: ScheduleCron, Expr: s, Display: s}, next, nil
	}
	return Schedule{}, time.Time{}, errors.New("schedule must be like 30m, every 2h, every monday 9am, 0 9 * * *, an RFC3339 one-shot, or in 30m")
}

// ScheduleNext computes the next UTC occurrence after now for a stored schedule.
func ScheduleNext(sched Schedule, now time.Time) (time.Time, error) {
	now = now.UTC()
	switch sched.Kind {
	case ScheduleOnce:
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(sched.RunAt))
		if err != nil {
			return time.Time{}, errors.New("one-shot run_at does not parse")
		}
		return t.UTC(), nil
	case ScheduleInterval:
		if sched.Minutes <= 0 {
			return time.Time{}, errors.New("interval minutes must be positive")
		}
		return now.Add(time.Duration(sched.Minutes) * time.Minute), nil
	case ScheduleCron:
		fields := strings.Fields(sched.Expr)
		if len(fields) != 5 {
			return time.Time{}, errors.New("cron expression must have 5 fields")
		}
		return nextCron(fields, now)
	default:
		return time.Time{}, errors.New("unknown schedule kind")
	}
}

// SchedulePeriod estimates the recurrence period for grace math: exact
// for intervals, measured from two cron occurrences, zero for one-shots.
func SchedulePeriod(sched Schedule, now time.Time) time.Duration {
	switch sched.Kind {
	case ScheduleInterval:
		return time.Duration(sched.Minutes) * time.Minute
	case ScheduleCron:
		fields := strings.Fields(sched.Expr)
		if len(fields) != 5 {
			return 24 * time.Hour
		}
		first, err := nextCron(fields, now.UTC())
		if err != nil {
			return 24 * time.Hour
		}
		second, err := nextCron(fields, first)
		if err != nil || !second.After(first) {
			return 24 * time.Hour
		}
		return second.Sub(first)
	default:
		return 0
	}
}

// durationMinutes renders a duration as whole minutes, minimum 1.
func durationMinutes(d time.Duration) int {
	m := int(d / time.Minute)
	if m < 1 {
		return 1
	}
	return m
}

func parseDurationWord(s string) (time.Duration, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	mult := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
	for _, unit := range []string{"w", "d", "h", "m", "s"} {
		if strings.HasSuffix(lower, unit) {
			num := strings.TrimSuffix(lower, unit)
			n, err := strconv.Atoi(strings.TrimSpace(num))
			if err != nil || n <= 0 {
				return 0, errors.New("invalid duration")
			}
			return time.Duration(n) * mult[unit], nil
		}
	}
	return 0, errors.New("invalid duration")
}

var weekdays = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
}

func parseEverySchedule(rest, display string, now time.Time) (Schedule, time.Time, error) {
	now = now.UTC()
	if d, err := parseDurationWord(rest); err == nil {
		next := now.Add(d)
		return Schedule{Kind: ScheduleInterval, Minutes: durationMinutes(d), Display: "every " + rest}, next, nil
	}
	fields := strings.Fields(strings.ToLower(rest))
	// every day 9am / every monday 9am / every monday
	day := ""
	timePart := ""
	if len(fields) >= 1 {
		day = fields[0]
	}
	if len(fields) >= 2 {
		timePart = strings.Join(fields[1:], " ")
	}
	if day == "day" || day == "daily" {
		hour, min, err := parseClock(timePart)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
		next := time.Date(now.Year(), now.Month(), now.Day()+1, hour, min, 0, 0, time.UTC)
		if candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, time.UTC); candidate.After(now) {
			next = candidate
		}
		return Schedule{Kind: ScheduleCron, Expr: fmt.Sprintf("%d %d * * *", min, hour), Display: display}, next, nil
	}
	weekday, ok := weekdays[day]
	if !ok {
		return Schedule{}, time.Time{}, errors.New("invalid every schedule")
	}
	hour, min := 9, 0
	if strings.TrimSpace(timePart) != "" {
		var err error
		hour, min, err = parseClock(timePart)
		if err != nil {
			return Schedule{}, time.Time{}, err
		}
	}
	next := nextWeekday(now.UTC(), weekday, hour, min)
	return Schedule{Kind: ScheduleCron, Expr: fmt.Sprintf("%d %d * * %d", min, hour, int(weekday)), Display: display}, next, nil
}

func parseClock(s string) (int, int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 9, 0, nil
	}
	meridiem := ""
	if strings.HasSuffix(s, "am") {
		meridiem = "am"
		s = strings.TrimSpace(strings.TrimSuffix(s, "am"))
	} else if strings.HasSuffix(s, "pm") {
		meridiem = "pm"
		s = strings.TrimSpace(strings.TrimSuffix(s, "pm"))
	}
	hour, min := 0, 0
	if strings.Contains(s, ":") {
		parts := strings.SplitN(s, ":", 2)
		var err error
		hour, err = strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, errors.New("invalid time")
		}
		min, err = strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, errors.New("invalid time")
		}
	} else {
		var err error
		hour, err = strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return 0, 0, errors.New("invalid time")
		}
	}
	if meridiem == "pm" && hour < 12 {
		hour += 12
	}
	if meridiem == "am" && hour == 12 {
		hour = 0
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return 0, 0, errors.New("invalid time")
	}
	return hour, min, nil
}

func nextWeekday(now time.Time, day time.Weekday, hour, min int) time.Time {
	days := (int(day) - int(now.Weekday()) + 7) % 7
	candidate := time.Date(now.Year(), now.Month(), now.Day()+days, hour, min, 0, 0, now.Location())
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate
}

type cronField struct {
	values map[int]bool
	any    bool
}

func parseCronField(field string, min, max int) (cronField, error) {
	out := cronField{values: map[int]bool{}}
	if field == "*" {
		out.any = true
		return out, nil
	}
	for _, part := range strings.Split(field, ",") {
		step := 1
		base := part
		if strings.Contains(part, "/") {
			pieces := strings.SplitN(part, "/", 2)
			base = pieces[0]
			var err error
			step, err = strconv.Atoi(pieces[1])
			if err != nil || step <= 0 {
				return out, errors.New("invalid cron step")
			}
		}
		lo, hi := min, max
		if base != "*" && base != "" {
			if strings.Contains(base, "-") {
				bounds := strings.SplitN(base, "-", 2)
				var err error
				lo, err = strconv.Atoi(bounds[0])
				if err != nil {
					return out, errors.New("invalid cron range")
				}
				hi, err = strconv.Atoi(bounds[1])
				if err != nil {
					return out, errors.New("invalid cron range")
				}
			} else {
				var err error
				lo, err = strconv.Atoi(base)
				if err != nil {
					return out, errors.New("invalid cron value")
				}
				hi = lo
			}
		}
		if lo < min || hi > max || lo > hi {
			return out, errors.New("cron value out of range")
		}
		for v := lo; v <= hi; v += step {
			out.values[v] = true
		}
	}
	return out, nil
}

func (f cronField) matches(v int) bool {
	if f.any {
		return true
	}
	return f.values[v]
}

func nextCron(fields []string, now time.Time) (time.Time, error) {
	minF, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return time.Time{}, err
	}
	hourF, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return time.Time{}, err
	}
	domF, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return time.Time{}, err
	}
	monthF, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return time.Time{}, err
	}
	dowF, err := parseCronField(fields[4], 0, 6)
	if err != nil {
		return time.Time{}, err
	}
	start := now.Truncate(time.Minute).Add(time.Minute)
	loc := start.Location()
	domRestricted := !domF.any
	dowRestricted := !dowF.any
	dateMatches := func(day time.Time) bool {
		if !monthF.matches(int(day.Month())) {
			return false
		}
		domMatch := domF.matches(day.Day())
		dowMatch := dowF.matches(int(day.Weekday()))
		if domRestricted && dowRestricted {
			return domMatch || dowMatch
		}
		return domMatch && dowMatch
	}
	// Day-first scan over ~8 years: covers Feb 29 across a skipped
	// century leap year (e.g. 2096 -> 2104) while staying cheap for
	// sparse dates. Calendar construction preserves wall-clock time
	// across DST transitions.
	const maxDays = 366 * 8
	for d := 0; d < maxDays; d++ {
		day := time.Date(start.Year(), start.Month(), start.Day()+d, 0, 0, 0, 0, loc)
		if !dateMatches(day) {
			continue
		}
		startHour, startMin := 0, 0
		if d == 0 {
			startHour = start.Hour()
			startMin = start.Minute()
		}
		for h := startHour; h < 24; h++ {
			if !hourF.matches(h) {
				continue
			}
			m0 := 0
			if d == 0 && h == startHour {
				m0 = startMin
			}
			for m := m0; m < 60; m++ {
				if !minF.matches(m) {
					continue
				}
				cand := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
				// Skip nonexistent wall times (spring-forward gap
				// normalizes to a different hour/minute).
				if cand.Hour() != h || cand.Minute() != m {
					continue
				}
				if !cand.After(now) {
					continue
				}
				return cand, nil
			}
		}
	}
	return time.Time{}, errors.New("no cron occurrence within 8 years")
}
