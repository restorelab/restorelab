package checks

import (
	"strings"
	"testing"
	"time"

	"github.com/restorelab/restorelab/internal/core"
)

func testTarget() core.Target {
	return core.Target{
		IP:         "10.0.0.5",
		WorkloadID: "101",
		Node:       "pve1",
		Name:       "restore-test",
		Vars:       map[string]string{"env": "staging"},
	}
}

func TestParams_TemplateExpansion(t *testing.T) {
	p := NewParams(map[string]any{
		"url": "http://{{ .ip }}:3000/health?env={{ .env }}",
	}, testTarget())

	got := p.String("url", "")
	want := "http://10.0.0.5:3000/health?env=staging"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParams_TemplateExpansion_UnknownKey(t *testing.T) {
	p := NewParams(map[string]any{
		"url": "http://{{ .doesnotexist }}:3000/",
	}, testTarget())

	_ = p.String("url", "")
	err := p.Err()
	if err == nil {
		t.Fatal("expected an error for a template referencing an unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "url") {
		t.Fatalf("error should mention the param key, got: %v", err)
	}
}

func TestParams_TemplateExpansion_NoTemplateNoop(t *testing.T) {
	p := NewParams(map[string]any{"host": "10.0.0.9"}, testTarget())
	if got := p.String("host", ""); got != "10.0.0.9" {
		t.Fatalf("String() = %q, want %q", got, "10.0.0.9")
	}
}

func TestParams_Int_Coercion(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int
	}{
		{"int", 22, 22},
		{"float64", float64(22), 22},
		{"string", "22", 22},
		{"int64", int64(22), 22},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParams(map[string]any{"port": tc.val}, testTarget())
			got := p.Int("port", -1)
			if got != tc.want {
				t.Fatalf("Int() = %d, want %d", got, tc.want)
			}
			if err := p.Err(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestParams_Int_Default(t *testing.T) {
	p := NewParams(map[string]any{}, testTarget())
	if got := p.Int("count", 3); got != 3 {
		t.Fatalf("Int() = %d, want 3", got)
	}
}

func TestParams_Int_BadValue(t *testing.T) {
	p := NewParams(map[string]any{"port": "not-a-number"}, testTarget())
	got := p.Int("port", 7)
	if got != 7 {
		t.Fatalf("Int() should return default on bad value, got %d", got)
	}
	if p.Err() == nil {
		t.Fatal("expected an error to be recorded")
	}
}

func TestParams_RequireInt_Missing(t *testing.T) {
	p := NewParams(map[string]any{}, testTarget())
	p.RequireInt("port")
	err := p.Err()
	if err == nil {
		t.Fatal("expected an error for a missing required int")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("error should mention the field name, got: %v", err)
	}
}

func TestParams_RequireString_Missing(t *testing.T) {
	p := NewParams(map[string]any{}, testTarget())
	p.RequireString("url")
	if p.Err() == nil {
		t.Fatal("expected an error for a missing required string")
	}
}

func TestParams_Bool(t *testing.T) {
	p := NewParams(map[string]any{"a": true, "b": "true", "c": "false"}, testTarget())
	if !p.Bool("a", false) {
		t.Fatal("expected a == true")
	}
	if !p.Bool("b", false) {
		t.Fatal("expected b == true (string coercion)")
	}
	if p.Bool("c", true) {
		t.Fatal("expected c == false (string coercion)")
	}
	if !p.Bool("missing", true) {
		t.Fatal("expected default true for missing key")
	}
	if err := p.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParams_Duration(t *testing.T) {
	p := NewParams(map[string]any{"interval": "500ms", "timeout": 5}, testTarget())
	if got := p.Duration("interval", time.Second); got != 500*time.Millisecond {
		t.Fatalf("Duration() = %v, want 500ms", got)
	}
	if got := p.Duration("timeout", time.Second); got != 5*time.Second {
		t.Fatalf("Duration() = %v, want 5s (bare number as seconds)", got)
	}
	if got := p.Duration("missing", 3*time.Second); got != 3*time.Second {
		t.Fatalf("Duration() = %v, want default 3s", got)
	}
}

func TestParams_Duration_Bad(t *testing.T) {
	p := NewParams(map[string]any{"interval": "not-a-duration"}, testTarget())
	got := p.Duration("interval", time.Second)
	if got != time.Second {
		t.Fatalf("Duration() should return default on bad value, got %v", got)
	}
	if p.Err() == nil {
		t.Fatal("expected an error to be recorded")
	}
}

func TestParams_StringSlice(t *testing.T) {
	p := NewParams(map[string]any{
		"expect":  []any{"1.2.3.4", "{{ .ip }}"},
		"strs":    []string{"a", "b"},
		"scalar":  "solo",
		"missing": nil,
	}, testTarget())

	got := p.StringSlice("expect")
	want := []string{"1.2.3.4", "10.0.0.5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("StringSlice(expect) = %v, want %v", got, want)
	}

	if got := p.StringSlice("scalar"); len(got) != 1 || got[0] != "solo" {
		t.Fatalf("StringSlice(scalar) = %v, want [solo]", got)
	}

	if got := p.StringSlice("nope"); got != nil {
		t.Fatalf("StringSlice(nope) = %v, want nil", got)
	}
}

func TestParams_Map(t *testing.T) {
	p := NewParams(map[string]any{
		"headers": map[string]any{"X-Env": "{{ .env }}"},
	}, testTarget())

	m := p.StringMap("headers")
	if m["X-Env"] != "staging" {
		t.Fatalf("StringMap(headers)[X-Env] = %q, want %q", m["X-Env"], "staging")
	}
}

func TestParams_AccumulatesMultipleErrors(t *testing.T) {
	p := NewParams(map[string]any{
		"port": "nope",
	}, testTarget())
	p.RequireString("url")
	p.RequireInt("port")

	err := p.Err()
	if err == nil {
		t.Fatal("expected accumulated errors")
	}
	msg := err.Error()
	if !strings.Contains(msg, "url") || !strings.Contains(msg, "port") {
		t.Fatalf("expected both url and port errors reported together, got: %v", msg)
	}
}

func TestParams_Any_JSONEquals(t *testing.T) {
	p := NewParams(map[string]any{"json_equals": true}, testTarget())
	v, ok := p.Any("json_equals")
	if !ok {
		t.Fatal("expected json_equals to be present")
	}
	if v != true {
		t.Fatalf("Any(json_equals) = %v, want true", v)
	}
}
