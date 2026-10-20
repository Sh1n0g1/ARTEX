package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the generic webhook template's **capability boundary**.
//
// This is the one place in the whole package where "a user-supplied string
// gets evaluated as code", so what it can and can't do must be made explicit
// and pinned down with tests -- otherwise someone could later casually add a
// method to the template context, or a readFile to the FuncMap, and the
// capability surface would silently expand while the diff looks like a
// harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can fetch either a field or
// call a method). So as long as the template context can reach **any** type
// with an exported method, that method is exposed to the template author.
// This feature's context is deliberately pure data (exported fields only,
// zero methods).
//
// If this fails: it means someone added a method to webhookTemplateData /
// webhookItem. Before deciding to allow it, think through whether that
// method could be used by a template to read something that shouldn't be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d method(s) (%s): text/template can call them, "+
				"which opens up those methods' capabilities to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to the
// template.
//
// Every extra function in FuncMap is one more capability. Currently only
// json / jsons exist, whose job is to serialize a value into a JSON
// fragment -- no file reads, no requests, no command execution.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the template function set has changed: got %v, expected %v. Before adding a function, confirm it doesn't "+
			"expand the capability surface (no file read/write, no network requests, no command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in the
// template: accessing something that doesn't exist must fail rather than echo
// back something; and the failure message must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a non-existent field should error")
	}
	// The error must not contain real content from the template context
	// (finding title/summary).
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a broken template is a config error,
// which retrying won't self-heal. If judged retryable, a bad template would
// waste three rounds of backoff on every single delivery.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be caught at save time")
	}
	// Even bypassing validation and delivering directly, it must still be
	// judged a permanent failure rather than retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be judged permanent, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "the template's
// render result must be valid JSON". It incidentally blocks "use the template
// to produce plain text and trigger some other protocol" style usage.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering out non-JSON must be rejected (rather than sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering out non-JSON should be judged permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error message should explain it's a JSON problem, got %v", err)
	}
}
