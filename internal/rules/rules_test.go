package rules

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T) Input {
	t.Helper()
	in, err := DecodeInput("MESSAGE_CREATE", json.RawMessage(`{
		"author":{"bot":false},"channel_id":"thread",
		"content":"こんにちは",
		"attachments":[
			{"id":"pdf1","filename":"A.PDF","content_type":"application/pdf","size":20},
			{"id":"png","filename":"b.png","content_type":"image/png","size":5},
			{"id":"pdf2","filename":"c.pdf","content_type":"application/pdf","size":8},
			{"id":"txt","filename":"d.txt","size":1}
		]
	}`), map[string]any{"channel": map[string]any{"parent_id": "forum"}})
	if err != nil {
		t.Fatal(err)
	}
	return in
}
func TestConditionsAndAttachmentEvidence(t *testing.T) {
	tests := []struct {
		source  string
		match   bool
		indices []int
		uses    bool
	}{
		{`event_type == "MESSAGE_CREATE" && context.channel.parent_id == "forum"`, true, []int{}, false},
		{`data.author.bot == false && data.webhook_id == null && data.attachments.some(a => a.filename.toLowerCase().endsWith(".pdf"))`, true, []int{0, 2}, true},
		{`data.attachments.some(a => a.content_type == "application/pdf") || data.attachments.some(a => a.content_type == "image/png")`, true, []int{0, 1, 2}, true},
		{`data.attachments.some(a => a.content_type == "application/pdf" || a.filename.endsWith(".pdf"))`, true, []int{0, 2}, true},
		{`data.attachments.some(a => a.content_type == "application/pdf" && a.size < 10)`, true, []int{2}, true},
		{`data.attachments.some(a => a.content_type == "application/pdf") && data.attachments.some(a => a.size < 10)`, true, []int{0, 1, 2, 3}, true},
		{`data.attachments.every(a => a.size > 0)`, true, []int{0, 1, 2, 3}, true},
		{`!data.attachments.every(a => a.content_type == "application/pdf")`, true, []int{1, 3}, true},
		{`data.attachments.some(a => !(a.content_type == "application/pdf"))`, true, []int{1, 3}, true},
		{`data.attachments.some((a) => a.filename.matches("(?i)\\.pdf$"))`, true, []int{0, 2}, true},
		{`["forum","another"].includes(context.channel.parent_id)`, true, []int{}, false},
		{`data.content.startsWith('こん') && data.content.includes("にち")`, true, []int{}, false},
		{`data.attachments.some(a => a.size == "8")`, false, []int{}, true},
		{`data.missing.array.some(a => true)`, false, []int{}, false},
		{`data.attachments[1].content_type == "image/png"`, true, []int{1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.source, func(t *testing.T) {
			p, err := Compile(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Evaluate(fixture(t))
			if err != nil {
				t.Fatal(err)
			}
			if r.Match != tc.match || r.UsesAttachments != tc.uses || !reflect.DeepEqual(r.AttachmentIndices, tc.indices) {
				t.Fatalf("got %#v", r)
			}
		})
	}
}
func TestInvalidPrograms(t *testing.T) {
	for _, source := range []string{
		`data.attachments.some(a => { return true; })`,
		`unknown == true`,
		`data.content.exec("code")`,
		`data.content.matches(data.content)`,
		`data.content.matches("[")`,
		`data.attachments.some(data => true)`,
		strings.Repeat("(", 100) + "true" + strings.Repeat(")", 100),
		strings.Repeat("a", MaxSourceBytes+1),
	} {
		if _, err := Compile(source); err == nil {
			t.Fatalf("accepted invalid program %s", source)
		}
	}
}
func TestLargeAttachmentListsAndBudget(t *testing.T) {
	files := make([]any, 130)
	for i := range files {
		files[i] = map[string]any{"size": float64(i)}
	}
	in := Input{Data: map[string]any{"attachments": files}, Context: map[string]any{}}
	p, err := Compile(`data.attachments.some(a => a.size >= 63 && a.size <= 65)`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Evaluate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.AttachmentIndices, []int{63, 64, 65}) {
		t.Fatal(r)
	}
	q, err := Compile(`data.attachments.every(a => data.attachments.every(b => data.attachments.every(c => true)))`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.Evaluate(in); err == nil {
		t.Fatal("unbounded work was allowed")
	}
}
func TestEvaluationIsConcurrentSafe(t *testing.T) {
	p, err := Compile(`data.attachments.some(a => a.size < 10)`)
	if err != nil {
		t.Fatal(err)
	}
	in := fixture(t)
	done := make(chan error, 20)
	for range 20 {
		go func() { _, err := p.Evaluate(in); done <- err }()
	}
	for range 20 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
func BenchmarkEvaluation(b *testing.B) {
	in, _ := DecodeInput("MESSAGE_CREATE", json.RawMessage(`{"author":{"bot":false},"attachments":[{"filename":"file.pdf","content_type":"application/pdf"}]}`), map[string]any{})
	p, _ := Compile(`event_type == "MESSAGE_CREATE" && data.author.bot == false && data.attachments.some(a => a.content_type == "application/pdf" || a.filename.toLowerCase().endsWith(".pdf"))`)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := p.Evaluate(in); err != nil {
			b.Fatal(err)
		}
	}
}
