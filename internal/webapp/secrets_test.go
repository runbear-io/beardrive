package webapp

import (
	"reflect"
	"strings"
	"testing"
)

// Fabricated, structurally-valid-looking strings. None is a real credential.
const (
	fakeAWSKey    = "AKIAIOSFODNN7EXAMPLE"
	fakeOpenAIKey = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	fakeGitHubPAT = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789ab"
	fakeSlackTok  = "xoxb-1234567890-abcdefghij"
	fakeGitLabPAT = "glpat-abcdefghij0123456789XY"
	fakePrivKey   = "-----BEGIN RSA PRIVATE KEY-----"
)

func TestScanSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  string
		want []secretFinding
	}{
		{"clean", "# Notes\n\nnothing to see, sk- is just a prefix here\n", nil},
		{"aws", "line1\nline2\nkey = " + fakeAWSKey + "\n", []secretFinding{{"aws_access_key_id", 3}}},
		{"openai", "OPENAI=" + fakeOpenAIKey, []secretFinding{{"openai_api_key", 1}}},
		{"github", "\n\n" + fakeGitHubPAT, []secretFinding{{"github_pat", 3}}},
		{"slack", "token: " + fakeSlackTok, []secretFinding{{"slack_token", 1}}},
		{"gitlab", "x\n" + fakeGitLabPAT, []secretFinding{{"gitlab_pat", 2}}},
		{"private key", "a\nb\nc\n" + fakePrivKey + "\nMIIE...\n", []secretFinding{{"private_key", 4}}},
		{
			// One line, one rule, three keys: one finding, not three.
			"multi key line deduped",
			"a=" + fakeAWSKey + " b=AKIAZZZZZZZZZZZZZZZZ c=AKIAYYYYYYYYYYYYYYYY",
			[]secretFinding{{"aws_access_key_id", 1}},
		},
		{
			"two rules same line",
			"env: " + fakeAWSKey + " " + fakeSlackTok,
			[]secretFinding{{"aws_access_key_id", 1}, {"slack_token", 1}},
		},
		{
			// A bufio.Scanner would blow its 64 KiB token limit here and report
			// nothing at all — which is why scanSecrets is byte-oriented.
			"no newline in a big buffer",
			strings.Repeat("x", 300_000) + fakeAWSKey,
			[]secretFinding{{"aws_access_key_id", 1}},
		},
		{
			"sorted by line then rule",
			fakeSlackTok + "\n" + fakeAWSKey,
			[]secretFinding{{"slack_token", 1}, {"aws_access_key_id", 2}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanSecrets([]byte(tc.buf))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scanSecrets = %v, want %v", got, tc.want)
			}
		})
	}
}
