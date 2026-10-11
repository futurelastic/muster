package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

const goodTaskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>DOMAIN\someone</UserId></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>C:\Users\someone\AppData\Local\Programs\muster\muster.exe</Command><Arguments>serve</Arguments></Exec>
  </Actions>
</Task>`

func utf16le(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func TestParseTaskXMLAcceptsEveryEncodingSchtasksProduces(t *testing.T) {
	be := []byte{0xFE, 0xFF}
	for _, u := range utf16.Encode([]rune(goodTaskXML)) {
		be = binary.BigEndian.AppendUint16(be, u)
	}
	for name, raw := range map[string][]byte{
		"utf-16le with a mark":    utf16le(goodTaskXML, true),
		"utf-16le without a mark": utf16le(goodTaskXML, false),
		"utf-16be with a mark":    be,
		"utf-8 with a mark":       append([]byte{0xEF, 0xBB, 0xBF}, goodTaskXML...),
		"8-bit text":              []byte(goodTaskXML),
	} {
		t.Run(name, func(t *testing.T) {
			info, err := parseTaskXML(raw)
			if err != nil {
				t.Fatal(err)
			}
			if info.ExecutionTimeLimit != "PT0S" || info.DisallowStartIfOnBatteries || info.StopIfGoingOnBatteries ||
				!info.LogonTrigger || info.Arguments != "serve" ||
				info.Command != `C:\Users\someone\AppData\Local\Programs\muster\muster.exe` {
				t.Errorf("parsed %+v", info)
			}
		})
	}
}

func TestParseTaskXMLRejectsText(t *testing.T) {
	if _, err := parseTaskXML([]byte("ERROR: The system cannot find the file specified.")); err == nil {
		t.Error("a message that is not XML parsed")
	}
}

func TestParseTaskXMLAbsentSettingsAreTheSchemaDefaults(t *testing.T) {
	info, err := parseTaskXML([]byte(`<Task><Settings></Settings></Task>`))
	if err != nil {
		t.Fatal(err)
	}
	// Absent battery settings are true in the schema, and no time limit is the
	// 72 hour default; neither may read as "fine".
	if !info.DisallowStartIfOnBatteries || !info.StopIfGoingOnBatteries || info.ExecutionTimeLimit != "" {
		t.Errorf("parsed %+v", info)
	}
}

func TestJudgeTask(t *testing.T) {
	const exe = `C:\Users\someone\AppData\Local\Programs\muster\muster.exe`
	good := taskInfo{ExecutionTimeLimit: "PT0S", LogonTrigger: true, Command: exe, Arguments: "serve"}

	for _, tc := range []struct {
		name   string
		mut    func(*taskInfo)
		exe    string
		want   rowStatus
		detail string
	}{
		{"all as documented", func(*taskInfo) {}, exe, statusPass, ""},
		{"path differs only in case", func(i *taskInfo) { i.Command = strings.ToUpper(exe) }, exe, statusPass, ""},
		{"72 hour default (no element)", func(i *taskInfo) { i.ExecutionTimeLimit = "" }, exe, statusFail, "72 hours"},
		{"an explicit limit", func(i *taskInfo) { i.ExecutionTimeLimit = "PT72H" }, exe, statusFail, "PT72H"},
		{"battery start refused", func(i *taskInfo) { i.DisallowStartIfOnBatteries = true }, exe, statusWarn, "battery"},
		{"stops on battery", func(i *taskInfo) { i.StopIfGoingOnBatteries = true }, exe, statusWarn, "battery"},
		{"another executable", func(*taskInfo) {}, `C:\elsewhere\muster.exe`, statusWarn, "different executable"},
		{"no action", func(i *taskInfo) { i.Command = "" }, exe, statusWarn, "no action"},
		{"no logon trigger", func(i *taskInfo) { i.LogonTrigger = false }, exe, statusWarn, "log-on trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := good
			tc.mut(&info)
			row := judgeTask(info, tc.exe)
			if row.ID != "service.task" || row.Status != tc.want {
				t.Fatalf("row = %+v, want status %s", row, tc.want)
			}
			if !strings.Contains(row.Summary+" "+row.Detail, tc.detail) {
				t.Errorf("row %+v does not mention %q", row, tc.detail)
			}
		})
	}
}
