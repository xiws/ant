package cli

import (
	"strings"
	"testing"
)

func TestFormatToolContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "read content",
			in:   `{"ok":true,"data":{"content":"     1\thello\n","path":"a.txt"}}`,
			want: []string{"hello"},
		},
		{
			name: "bash stdout stderr exit",
			in:   `{"ok":true,"data":{"stdout":"ok\n","stderr":"warn\n","exitCode":2}}`,
			want: []string{"ok", "[stderr]", "warn", "[exit] 2"},
		},
		{
			name: "tool error",
			in:   `{"ok":false,"error":{"code":"not_found","message":"missing","hint":"check path"}}`,
			want: []string{"not_found", "missing", "check path"},
		},
		{
			name: "plain json",
			in:   `{"ok":true,"data":{"path":"a.txt","bytes":3}}`,
			want: []string{"\"path\": \"a.txt\"", "\"bytes\": 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := formatToolContent([]byte(tc.in))
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("formatToolContent missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestBoundText(t *testing.T) {
	got, clipped := boundText("a\nb\nc", 100, 2)
	if !clipped || got != "a\nb\n…" {
		t.Fatalf("boundText = %q/%v", got, clipped)
	}
}
