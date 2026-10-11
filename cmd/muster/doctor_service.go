package main

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// The service.task doctor row reads what a scheduled task says about itself, so
// the parts of it that are not Windows-specific — decoding what `schtasks
// /Query /XML` printed, parsing it, and judging it — live here, where every
// platform's tests can reach them. Running schtasks is doctor_service_windows.go.
//
// # What a correct task looks like
//
// docs/install.md registers the service as a task with an at-log-on trigger for
// the user, an interactive-token limited principal, no execution time limit,
// battery settings off, restart on failure, "ignore new" for a second instance,
// and the action `muster.exe serve`. Each of those is there because the default
// is wrong for a service. The one that is silent and fatal is the time limit:
// Task Scheduler's default is 72 hours, after which it ends the task, and the
// service stops with no error anywhere.

// taskInfo is what the row needs from a task's XML.
type taskInfo struct {
	// ExecutionTimeLimit is as written; "" means the element is absent, which
	// Task Scheduler reads as PT72H.
	ExecutionTimeLimit string
	// DisallowStartIfOnBatteries and StopIfGoingOnBatteries are the two
	// settings that keep a service from running on a laptop's battery. Absent
	// means true in the task schema, and the parser says so.
	DisallowStartIfOnBatteries bool
	StopIfGoingOnBatteries     bool
	LogonTrigger               bool
	// Command is the first action's executable path, unquoted.
	Command   string
	Arguments string
}

type taskXML struct {
	XMLName  xml.Name `xml:"Task"`
	Triggers struct {
		Logon []struct{} `xml:"LogonTrigger"`
	} `xml:"Triggers"`
	Settings struct {
		ExecutionTimeLimit         *string `xml:"ExecutionTimeLimit"`
		DisallowStartIfOnBatteries *string `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     *string `xml:"StopIfGoingOnBatteries"`
	} `xml:"Settings"`
	Actions struct {
		Exec []struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

// decodeTaskText turns the bytes schtasks wrote into text the XML parser can
// read. What it writes depends on how it was run: UTF-16 with a byte-order mark
// to a file, UTF-16 without one on some systems, the console code page to a
// pipe — and in every case the XML declaration says UTF-16 regardless. So the
// declaration is not trusted: the bytes are looked at.
func decodeTaskText(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		return string(raw[3:])
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		return decodeUTF16(raw[2:], binary.LittleEndian)
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return decodeUTF16(raw[2:], binary.BigEndian)
	}
	// No mark. A text that is mostly ASCII in UTF-16 has a NUL in every other
	// byte; plain 8-bit text has none.
	if len(raw) >= 2 && bytes.IndexByte(raw, 0) >= 0 {
		if raw[0] == 0 {
			return decodeUTF16(raw, binary.BigEndian)
		}
		return decodeUTF16(raw, binary.LittleEndian)
	}
	return string(raw)
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order.Uint16(b[i:]))
	}
	return string(utf16.Decode(u))
}

// parseTaskXML reads a task definition as `schtasks /Query /XML` prints it.
func parseTaskXML(raw []byte) (taskInfo, error) {
	text := decodeTaskText(raw)
	// Anything before the root is noise (a leftover mark, a blank line).
	if i := strings.Index(text, "<"); i > 0 {
		text = text[i:]
	}
	dec := xml.NewDecoder(strings.NewReader(text))
	// The text has already been decoded; the declaration's encoding name is
	// whatever the tool wrote and no longer describes it.
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	var t taskXML
	if err := dec.Decode(&t); err != nil {
		return taskInfo{}, fmt.Errorf("the task definition did not parse as XML: %w", err)
	}
	boolOr := func(p *string, def bool) bool {
		if p == nil {
			return def
		}
		return strings.EqualFold(strings.TrimSpace(*p), "true")
	}
	info := taskInfo{
		DisallowStartIfOnBatteries: boolOr(t.Settings.DisallowStartIfOnBatteries, true),
		StopIfGoingOnBatteries:     boolOr(t.Settings.StopIfGoingOnBatteries, true),
		LogonTrigger:               len(t.Triggers.Logon) > 0,
	}
	if t.Settings.ExecutionTimeLimit != nil {
		info.ExecutionTimeLimit = strings.TrimSpace(*t.Settings.ExecutionTimeLimit)
	}
	if len(t.Actions.Exec) > 0 {
		info.Command = strings.Trim(strings.TrimSpace(t.Actions.Exec[0].Command), `"`)
		info.Arguments = strings.TrimSpace(t.Actions.Exec[0].Arguments)
	}
	return info, nil
}

// samePath compares two Windows paths: case-insensitively, after cleaning.
func samePath(a, b string) bool {
	norm := func(p string) string {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(p, "/", `\`)))
	}
	return norm(a) == norm(b)
}

// judgeTask turns a parsed task into the service.task row. exe is the
// executable doing the checking, which is what the task is expected to run.
func judgeTask(info taskInfo, exe string) doctorRow {
	row := doctorRow{ID: "service.task"}

	// A time limit is the one thing here that stops the service without saying
	// so, so it is a failure and not a warning.
	switch info.ExecutionTimeLimit {
	case "PT0S":
	case "":
		row.Status = statusFail
		row.Summary = "the task has no execution time limit set, which Task Scheduler reads as 72 hours — it will end the service after that"
		row.Detail = "set the limit to PT0S (no limit) in the task's settings; docs/install.md has the definition"
		return row
	default:
		row.Status = statusFail
		row.Summary = fmt.Sprintf("the task's execution time limit is %s, not PT0S — Task Scheduler will end the service when it runs out", info.ExecutionTimeLimit)
		row.Detail = "set the limit to PT0S (no limit) in the task's settings; docs/install.md has the definition"
		return row
	}

	var notes []string
	if info.DisallowStartIfOnBatteries || info.StopIfGoingOnBatteries {
		notes = append(notes, "the task is set to not start, or to stop, on battery power — turn both battery settings off")
	}
	if info.Command == "" {
		notes = append(notes, "the task has no action")
	} else if exe != "" && !samePath(info.Command, exe) {
		notes = append(notes, "the task runs a different executable than this one — after an upgrade that is the old build")
	}
	if !info.LogonTrigger {
		notes = append(notes, "the task has no at-log-on trigger, so it will not start when the user signs in")
	}
	if len(notes) > 0 {
		row.Status = statusWarn
		row.Summary = "the scheduled task exists with no time limit, but it is not set up as the install guide describes"
		row.Detail = strings.Join(notes, "; ")
		return row
	}
	row.Status = statusPass
	row.Summary = "a scheduled task runs this executable at log-on with no execution time limit"
	return row
}
