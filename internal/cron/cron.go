// Package cron owns per-profile scheduled jobs: validation, file-backed
// storage, schedule parsing, tick dispatch, and an execution ledger.
//
// Storage is filesystem JSON under <profile>/cron/ (0600 files, 0700
// dirs), excluded from durable backups like other cache state. All
// mutations hold an flock on jobs.lock so model-triggered edits and the
// background ticker serialize across instances.
package cron

import (
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

// Job states for the scheduling state machine.
const (
	StateScheduled = "scheduled"
	StatePaused    = "paused"
	StateCompleted = "completed"
	StateRunning   = "running"
)

// RepeatState counts bounded runs: Times <= 0 repeats forever.
type RepeatState struct {
	Times     int `json:"times"`
	Completed int `json:"completed"`
}

// PendingSlot marks a dispatched-but-unfinished occurrence for restore-once.
type PendingSlot struct {
	Instant     int64 `json:"instant"`
	ClaimedUnix int64 `json:"claimed_unix"`
}

// Job is one scheduled unit of work.
type Job struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"`
	Schedule        string      `json:"schedule"`
	Prompt          string      `json:"prompt"`
	Skills          []string    `json:"skills,omitempty"`
	Script          string      `json:"script,omitempty"`
	MonitorURL      string      `json:"monitor_url,omitempty"`
	MonitorScript   string      `json:"monitor_script,omitempty"`
	BaselineHash    string      `json:"baseline_hash,omitempty"`
	NoAgent         bool        `json:"no_agent"`
	Deliver         string      `json:"deliver,omitempty"`
	ContextFrom     []string    `json:"context_from,omitempty"`
	Continuity      bool        `json:"continuity"`
	EnabledToolsets []string    `json:"enabled_toolsets,omitempty"`
	Workdir         string      `json:"workdir,omitempty"`
	AttachToSession string      `json:"attach_to_session,omitempty"`
	Repeat          RepeatState `json:"repeat"`
	CatchUp         *bool       `json:"catch_up,omitempty"`
	Paused          bool        `json:"paused"`
	State           string      `json:"state,omitempty"`
	OriginGateway   string      `json:"origin_gateway,omitempty"`
	OriginSession   string      `json:"origin_session,omitempty"`
	PausedReason    string      `json:"paused_reason,omitempty"`
	FailureCount    int         `json:"failure_count"`
	Pending         PendingSlot `json:"pending_slot"`
	LastInstant     int64       `json:"last_instant"`
	NextRunUnix     int64       `json:"next_run_at"`
	LastRunUnix     int64       `json:"last_run_at"`
	LastStatus      string      `json:"last_status,omitempty"`
	LastError       string      `json:"last_delivery_error,omitempty"`
	CreatedUnix     int64       `json:"created_unix"`
	UpdatedUnix     int64       `json:"updated_unix"`
}

// UnmarshalJSON keeps old jobs readable: a legacy boolean "repeat" meant
// catch-up behavior, and "paused" implies the paused state.
func (j *Job) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	repeatRaw := raw["repeat"]
	delete(raw, "repeat")
	rest, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	type plain Job
	if err := json.Unmarshal(rest, (*plain)(j)); err != nil {
		return err
	}
	if len(repeatRaw) > 0 {
		var legacy bool
		if err := json.Unmarshal(repeatRaw, &legacy); err == nil {
			j.CatchUp = &legacy
		} else if err := json.Unmarshal(repeatRaw, &j.Repeat); err != nil {
			return err
		}
	}
	if j.State == "" {
		j.State = StateScheduled
		if j.Paused {
			j.State = StatePaused
		}
	}
	j.Paused = j.State == StatePaused
	// Delivery is always populated: legacy jobs stored before the
	// origin-only contract default to the creating chat.
	if strings.TrimSpace(j.Deliver) == "" {
		j.Deliver = "origin"
	}
	return nil
}

// EffectivePaused reports whether a job must never fire.
func (j *Job) EffectivePaused() bool {
	return j.State == StatePaused || j.State == StateCompleted
}

// Execution is one ledger entry.
type Execution struct {
	JobID         string `json:"job_id"`
	Instant       int64  `json:"scheduled_instant"`
	StartedUnix   int64  `json:"started_unix"`
	FinishedUnix  int64  `json:"finished_unix"`
	Status        string `json:"status"`
	OutputPreview string `json:"output_preview,omitempty"`
	Delivered     bool   `json:"delivered"`
	DeliveryError string `json:"delivery_error,omitempty"`
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
	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, err
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
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].CreatedUnix > jobs[j].CreatedUnix })
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

// ValidateDeliver checks delivery targets. Origin (the creating chat) is
// the only supported target; the tool keeps the deliver parameter so future
// targets can be added without a schema change.
func ValidateDeliver(target string) error {
	target = strings.TrimSpace(target)
	if target == "origin" {
		return nil
	}
	return errors.New("invalid deliver target: origin is the only delivery target")
}

// Parsed is a resolved schedule.
type Parsed struct {
	Next   time.Time
	Period time.Duration
	Repeat bool
}

// ParseSchedule resolves human and cron schedules.
func ParseSchedule(raw string, now time.Time) (Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Parsed{}, errors.New("schedule is required")
	}
	lower := strings.ToLower(s)
	if rest, ok := strings.CutPrefix(lower, "in "); ok {
		d, err := parseDurationWord(strings.TrimSpace(rest))
		if err != nil {
			return Parsed{}, err
		}
		return Parsed{Next: now.Add(d), Repeat: false}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !t.After(now) {
			return Parsed{}, errors.New("one-shot schedule must be in the future")
		}
		return Parsed{Next: t, Repeat: false}, nil
	}
	if rest, ok := strings.CutPrefix(lower, "every "); ok {
		return parseEvery(strings.TrimSpace(rest), now)
	}
	if d, err := parseDurationWord(lower); err == nil {
		return Parsed{Next: now.Add(d), Period: d, Repeat: true}, nil
	}
	if fields := strings.Fields(s); len(fields) == 5 {
		next, err := nextCron(fields, now)
		if err != nil {
			return Parsed{}, err
		}
		return Parsed{Next: next, Period: 24 * time.Hour, Repeat: true}, nil
	}
	return Parsed{}, errors.New("schedule must be like 30m, every 2h, every monday 9am, 0 9 * * *, an RFC3339 one-shot, or in 30m")
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

func parseEvery(rest string, now time.Time) (Parsed, error) {
	if d, err := parseDurationWord(rest); err == nil {
		return Parsed{Next: now.Add(d), Period: d, Repeat: true}, nil
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
			return Parsed{}, err
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location()).Add(24 * time.Hour)
		if candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location()); candidate.After(now) {
			next = candidate
		}
		return Parsed{Next: next, Period: 24 * time.Hour, Repeat: true}, nil
	}
	weekday, ok := weekdays[day]
	if !ok {
		return Parsed{}, errors.New("invalid every schedule")
	}
	hour, min := 9, 0
	if strings.TrimSpace(timePart) != "" {
		var err error
		hour, min, err = parseClock(timePart)
		if err != nil {
			return Parsed{}, err
		}
	}
	next := nextWeekday(now, weekday, hour, min)
	return Parsed{Next: next, Period: 7 * 24 * time.Hour, Repeat: true}, nil
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
		candidate = candidate.Add(7 * 24 * time.Hour)
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
	candidate := now.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if minF.matches(candidate.Minute()) && hourF.matches(candidate.Hour()) && domF.matches(candidate.Day()) && monthF.matches(int(candidate.Month())) && dowF.matches(int(candidate.Weekday())) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, errors.New("no cron occurrence within a year")
}

// SchedulePeriod estimates the recurrence period for grace math: exact
// for durations and every-phrases, measured from two cron occurrences,
// zero for one-shots.
func SchedulePeriod(expr string, now time.Time) time.Duration {
	lower := strings.ToLower(strings.TrimSpace(expr))
	if rest, ok := strings.CutPrefix(lower, "in "); ok {
		if _, err := parseDurationWord(strings.TrimSpace(rest)); err == nil {
			return 0
		}
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(expr)); err == nil {
		return 0
	}
	if rest, ok := strings.CutPrefix(lower, "every "); ok {
		if d, err := parseDurationWord(strings.TrimSpace(rest)); err == nil {
			return d
		}
		fields := strings.Fields(strings.TrimSpace(rest))
		if len(fields) > 0 && (fields[0] == "day" || fields[0] == "daily") {
			return 24 * time.Hour
		}
		return 7 * 24 * time.Hour
	}
	if d, err := parseDurationWord(lower); err == nil {
		return d
	}
	if fields := strings.Fields(strings.TrimSpace(expr)); len(fields) == 5 {
		first, err := nextCron(fields, now)
		if err != nil {
			return 24 * time.Hour
		}
		second, err := nextCron(fields, first)
		if err != nil || !second.After(first) {
			return 24 * time.Hour
		}
		return second.Sub(first)
	}
	return 24 * time.Hour
}
