package main

import "testing"

func TestNotifySendArgs(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		summary, body string
	}{
		{[]string{"Build done"}, "Build done", ""},
		{[]string{"-u", "critical", "-t", "5000", "make", "all targets built"}, "make", "all targets built"},
		{[]string{"--app-name=ci", "-e", "-i", "dialog-information", "title", "body", "more"}, "title", "body more"},
		{[]string{"-a", "x", "--", "-dash", "body"}, "-dash", "body"},
	} {
		s, b, err := notifySendArgs(tc.args)
		if err != nil || s != tc.summary || b != tc.body {
			t.Errorf("%q → %q, %q, %v; want %q, %q", tc.args, s, b, err, tc.summary, tc.body)
		}
	}
	if _, _, err := notifySendArgs([]string{"-u", "low"}); err == nil {
		t.Error("no summary accepted")
	}
}
