package workspace

import (
	"path/filepath"
	"regexp"
	"strings"
)

// classifyFetch verdicts for curl/wget invocations.
const (
	fetchAllow = "allow"
	fetchDeny  = "deny"
	fetchSkip  = "skip" // not a fetch invocation
)

var (
	reFetchURL     = regexp.MustCompile(`\bhttps?://[^\s'"<>]+`)
	reUserinfo     = regexp.MustCompile(`\bhttps?://[^/\s]*:[^/\s]*@`)
	reURLSecret    = regexp.MustCompile(`(?i)\bhttps?://\S*(?:token=|api[_-]?key=|secret=|password=|auth_token=)`)
	reInsecure     = regexp.MustCompile(`\bcurl\b[^;&|]*--insecure\b|\bcurl\b[^;&|]*\s-k[\s]`)
	reSecretBody   = regexp.MustCompile(`(?i)(?:^|[\s;|&])(?:-d|--data(?:-binary|-urlencode)?)\b[^;&|]*(?:token=|api[_-]?key=|secret=|password=)`)
	reUploadFile   = regexp.MustCompile(`(?:^|[\s;|&])(?:--data\s+@\S+|--upload-file\s+\S+|\s-T\s+\S+)`)
	reRecursiveGet = regexp.MustCompile(`\bwget\b[^;&|]*\s-r[\s]`)
	reOutputTo     = regexp.MustCompile(`(?:^|[\s;|&])(?:-o\s+(\S+)|--output\s+(\S+)|-O\b)`)
	reWgetOutput   = regexp.MustCompile(`\bwget\b[^;&|]*-O\s+(\S+)`)
	reWgetPrefix   = regexp.MustCompile(`\bwget\b[^;&|]*-P\s+(\S+)`)
	reExecPipe     = regexp.MustCompile(`\|\s*(?:sh|bash|dash|zsh|sudo\s+sh|sudo\s+bash|python3?|perl|ruby|php)\b`)
	reEvalFetch    = regexp.MustCompile(`\beval\b[^;&|]*\bcurl\b|\beval\b[^;&|]*\bwget\b`)
	reDataFlag     = regexp.MustCompile(`(?i)(?:^|[\s;|&])(-d|--data(?:-binary|-urlencode)?)\s+`)
)

var textPipeAllow = map[string]bool{
	"jq": true, "grep": true, "head": true, "tail": true, "wc": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "less": true, "more": true,
}

// classifyFetch judges curl/wget invocations. Path- and data-sensitive
// checks run on prose-blanked text (echo/printf prose removed, real flag
// operands and quoted URLs visible); structural checks run on live text.
func classifyFetch(command, live, workdir string) (string, string) {
	if !fetchMentions(live) {
		return fetchSkip, ""
	}
	quoted := proseBlanked(normalizeCommand(command))
	urls := reFetchURL.FindAllString(quoted, -1)
	if len(urls) == 0 {
		return fetchAllow, ""
	}
	for _, url := range urls {
		if verdict, reason := judgeURL(url); verdict == fetchDeny {
			return verdict, reason
		}
	}
	if reUserinfo.MatchString(quoted) || reURLSecret.MatchString(quoted) {
		return fetchDeny, "credentials in fetch URL"
	}
	if reInsecure.MatchString(live) {
		return fetchDeny, "disabled TLS verification"
	}
	if reSecretBody.MatchString(live) {
		return fetchDeny, "secret-bearing request body"
	}
	if reason, hit := dataOperandSecrets(quoted); hit {
		return fetchDeny, reason
	}
	if reUploadFile.MatchString(live) {
		return fetchDeny, "outbound file upload"
	}
	if reRecursiveGet.MatchString(live) {
		return fetchDeny, "recursive download"
	}
	if reEvalFetch.MatchString(live) || reExecPipe.MatchString(live) {
		return fetchDeny, "execution of fetched content"
	}
	if verdict, reason := judgePipes(live); verdict == fetchDeny {
		return verdict, reason
	}
	for _, dest := range fetchDestinations(quoted) {
		if verdict, reason := judgeDestination(dest, workdir); verdict == fetchDeny {
			return verdict, reason
		}
	}
	return fetchAllow, ""
}

// dataOperandSecrets extracts -d/--data operands quote-aware and reports
// secret-bearing or file-sourced bodies. Echo/printf prose never reaches
// here; live flag operands always do.
func dataOperandSecrets(quoted string) (string, bool) {
	for _, loc := range reDataFlag.FindAllStringIndex(quoted, -1) {
		rest := strings.TrimSpace(quoted[loc[1]:])
		if rest == "" {
			continue
		}
		var operand string
		if rest[0] == '"' || rest[0] == '\'' {
			end := strings.IndexByte(rest[1:], rest[0])
			if end < 0 {
				operand = rest[1:]
			} else {
				operand = rest[1 : 1+end]
			}
		} else if fields := strings.Fields(rest); len(fields) > 0 {
			operand = strings.Trim(fields[0], ";|&")
		}
		if strings.HasPrefix(operand, "@") {
			return "outbound file upload", true
		}
		lower := strings.ToLower(operand)
		if strings.Contains(lower, "token=") || strings.Contains(lower, "api_key=") ||
			strings.Contains(lower, "api-key=") || strings.Contains(lower, "secret=") ||
			strings.Contains(lower, "password=") {
			return "secret-bearing request body", true
		}
	}
	return "", false
}

func fetchMentions(live string) bool {
	return strings.Contains(live, "curl") || strings.Contains(live, "wget")
}

// judgeURL denies plain HTTP off loopback. Loopback stays allowed so local
// dev-server health checks keep working.
func judgeURL(url string) (string, string) {
	lower := strings.ToLower(url)
	if strings.HasPrefix(lower, "http://") {
		rest := lower[len("http://"):]
		host := rest
		if i := strings.IndexAny(rest, "/:?"); i >= 0 {
			host = rest[:i]
		}
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return fetchAllow, ""
		}
		return fetchDeny, "plain-HTTP fetch"
	}
	return fetchAllow, ""
}

// judgePipes allows stdout and text-processor/pager pipes only.
func judgePipes(live string) (string, string) {
	segments := strings.Split(live, "|")
	for _, segment := range segments[1:] {
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		prog := filepath.Base(fields[0])
		if textPipeAllow[prog] {
			continue
		}
		if prog == "tee" {
			for _, token := range fields[1:] {
				token = strings.Trim(token, "\"'();")
				if strings.HasPrefix(token, "-") || token == "" {
					continue
				}
				return fetchDeny, "fetch piped to tee outside the workspace"
			}
			continue
		}
		return fetchDeny, "fetch piped to " + prog
	}
	return fetchAllow, ""
}

func fetchDestinations(live string) []string {
	var dests []string
	for _, match := range reOutputTo.FindAllStringSubmatch(live, -1) {
		for _, group := range match[1:] {
			if group != "" {
				dests = append(dests, group)
			}
		}
	}
	for _, match := range reWgetOutput.FindAllStringSubmatch(live, -1) {
		if len(match) > 1 && match[1] != "" {
			dests = append(dests, match[1])
		}
	}
	for _, match := range reWgetPrefix.FindAllStringSubmatch(live, -1) {
		if len(match) > 1 && match[1] != "" {
			dests = append(dests, match[1])
		}
	}
	return dests
}

// judgeDestination requires fetch outputs inside the working directory.
func judgeDestination(dest, workdir string) (string, string) {
	dest = strings.Trim(dest, "\"'();")
	var abs string
	if filepath.IsAbs(dest) {
		abs = filepath.Clean(dest)
	} else {
		abs = filepath.Join(workdir, dest)
	}
	rel, err := filepath.Rel(workdir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fetchDeny, "fetch destination outside the working directory"
	}
	return fetchAllow, ""
}
