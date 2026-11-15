package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"regexp"
	"strings"
)

type Input struct {
	EventType string
	Data      any
	Context   map[string]any
}
type Result struct {
	Match             bool  `json:"match"`
	UsesAttachments   bool  `json:"uses_attachments"`
	AttachmentIndices []int `json:"attachment_indices"`
}
type capture struct {
	low  uint64
	high []uint64
}

func bit(i int) capture {
	if i < 64 {
		return capture{low: uint64(1) << i}
	}
	w := (i - 64) / 64
	h := make([]uint64, w+1)
	h[w] = uint64(1) << ((i - 64) % 64)
	return capture{high: h}
}
func union(a, b capture) capture {
	out := capture{low: a.low | b.low}
	if len(a.high) == 0 {
		out.high = b.high
		return out
	}
	if len(b.high) == 0 {
		out.high = a.high
		return out
	}
	n := max(len(a.high), len(b.high))
	out.high = make([]uint64, n)
	for i := 0; i < n; i++ {
		if i < len(a.high) {
			out.high[i] |= a.high[i]
		}
		if i < len(b.high) {
			out.high[i] |= b.high[i]
		}
	}
	return out
}
func (c capture) indices(limit int) []int {
	out := []int{}
	add := func(w uint64, base int) {
		for w != 0 {
			n := bits.TrailingZeros64(w)
			if base+n < limit {
				out = append(out, base+n)
			}
			w &= w - 1
		}
	}
	add(c.low, 0)
	for i, w := range c.high {
		add(w, 64+i*64)
	}
	return out
}

type value struct {
	raw                       any
	origin                    capture
	yes, no                   capture
	booleanEvidence           bool
	dataRoot, attachmentArray bool
}

func boolValue(b bool, origin capture) value {
	v := value{raw: b, booleanEvidence: true}
	if b {
		v.yes = origin
	} else {
		v.no = origin
	}
	return v
}
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	case json.Number:
		n, _ := x.Float64()
		return n != 0
	default:
		return true
	}
}
func (v value) boolean() (bool, capture, capture) {
	if v.booleanEvidence {
		return v.raw.(bool), v.yes, v.no
	}
	b := truthy(v.raw)
	if b {
		return true, v.origin, capture{}
	}
	return false, capture{}, v.origin
}
func (v value) effectiveOrigin() capture {
	if v.booleanEvidence {
		return union(v.yes, v.no)
	}
	return v.origin
}
func arrayLength(v value) (int, bool) {
	switch x := v.raw.(type) {
	case []any:
		return len(x), true
	case []value:
		return len(x), true
	}
	return 0, false
}
func arrayAt(v value, i int) value {
	n, ok := arrayLength(v)
	if !ok || i < 0 || i >= n {
		return value{origin: v.origin}
	}
	var out value
	switch x := v.raw.(type) {
	case []any:
		out = value{raw: x[i]}
	case []value:
		out = x[i]
	}
	if v.attachmentArray {
		out.origin = union(out.origin, bit(i))
	} else {
		out.origin = union(out.origin, v.origin)
	}
	return out
}
func field(v value, name string) value {
	if obj, ok := v.raw.(map[string]any); ok {
		out := value{raw: obj[name], origin: v.origin}
		if v.dataRoot && name == "attachments" {
			out.attachmentArray = true
			if n, ok := arrayLength(out); ok {
				for i := 0; i < n; i++ {
					out.origin = union(out.origin, bit(i))
				}
			}
		}
		return out
	}
	if name == "length" {
		if n, ok := arrayLength(v); ok {
			return value{raw: float64(n), origin: v.origin}
		}
		if s, ok := v.raw.(string); ok {
			return value{raw: float64(len([]rune(s))), origin: v.origin}
		}
	}
	return value{origin: v.origin}
}

type env struct {
	input  Input
	locals []value
	steps  int
	err    error
}
type evalFn func(*env) value

func guarded(f evalFn) evalFn {
	return func(e *env) value {
		if e.err != nil {
			return value{}
		}
		e.steps++
		if e.steps > MaxEvaluationSteps {
			e.err = errors.New("condition evaluation budget exceeded")
			return value{}
		}
		return f(e)
	}
}

type Program struct {
	Source          string
	UsesAttachments bool
	UsesContext     bool
	run             evalFn
	slots           int
}
type compiler struct {
	slots       int
	attachments bool
	context     bool
}

func Compile(source string) (*Program, error) {
	n, err := parse(source)
	if err != nil {
		return nil, err
	}
	c := compiler{}
	f, err := c.compile(n, map[string]int{}, true, false)
	if err != nil {
		return nil, err
	}
	return &Program{Source: source, UsesAttachments: c.attachments, UsesContext: c.context, run: f, slots: c.slots}, nil
}
func (p *Program) Evaluate(in Input) (Result, error) {
	e := env{input: in, locals: make([]value, p.slots)}
	v := p.run(&e)
	b, yes, _ := v.boolean()
	if e.err != nil {
		return Result{}, e.err
	}
	count := 0
	if d, ok := in.Data.(map[string]any); ok {
		if a, ok := d["attachments"].([]any); ok {
			count = len(a)
		}
	}
	selected := []int{}
	if b {
		selected = yes.indices(count)
	}
	return Result{Match: b, UsesAttachments: p.UsesAttachments, AttachmentIndices: selected}, nil
}
func DecodeInput(eventType string, raw json.RawMessage, context map[string]any) (Input, error) {
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Input{}, err
	}
	return Input{EventType: eventType, Data: data, Context: context}, nil
}
func numeric(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	}
	return 0, false
}
func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if x, ok := numeric(a); ok {
		y, ok := numeric(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}
func compare(op string, a, b any) bool {
	switch op {
	case "==", "===":
		return equal(a, b)
	case "!=", "!==":
		return !equal(a, b)
	}
	x, ok := numeric(a)
	if ok {
		y, ok := numeric(b)
		if !ok {
			return false
		}
		switch op {
		case "<":
			return x < y
		case "<=":
			return x <= y
		case ">":
			return x > y
		case ">=":
			return x >= y
		}
	}
	if x, ok := a.(string); ok {
		y, ok := b.(string)
		if !ok {
			return false
		}
		switch op {
		case "<":
			return x < y
		case "<=":
			return x <= y
		case ">":
			return x > y
		case ">=":
			return x >= y
		}
	}
	return false
}
func (c *compiler) fail(n *node, message string) (evalFn, error) {
	return nil, &SyntaxError{n.pos, message}
}
func (c *compiler) rootData(n *node, locals map[string]int) bool {
	if n.kind != "variable" || n.text != "data" {
		return false
	}
	_, shadowed := locals["data"]
	return !shadowed
}
func (c *compiler) compile(n *node, locals map[string]int, wantTrue, wantFalse bool) (evalFn, error) {
	switch n.kind {
	case "literal":
		v := value{raw: n.value}
		return guarded(func(*env) value { return v }), nil
	case "variable":
		if slot, ok := locals[n.text]; ok {
			return guarded(func(e *env) value { return e.locals[slot] }), nil
		}
		switch n.text {
		case "event_type":
			return guarded(func(e *env) value { return value{raw: e.input.EventType} }), nil
		case "data":
			return guarded(func(e *env) value { return value{raw: e.input.Data, dataRoot: true} }), nil
		case "context":
			c.context = true
			return guarded(func(e *env) value { return value{raw: e.input.Context} }), nil
		}
		return c.fail(n, "unknown identifier "+n.text)
	case "property":
		if c.rootData(n.left, locals) && n.text == "attachments" {
			c.attachments = true
		}
		f, err := c.compile(n.left, locals, true, true)
		if err != nil {
			return nil, err
		}
		return guarded(func(e *env) value { return field(f(e), n.text) }), nil
	case "index":
		if c.rootData(n.left, locals) && n.right.kind == "literal" && n.right.value == "attachments" {
			c.attachments = true
		}
		left, err := c.compile(n.left, locals, true, true)
		if err != nil {
			return nil, err
		}
		right, err := c.compile(n.right, locals, true, true)
		if err != nil {
			return nil, err
		}
		return guarded(func(e *env) value {
			a, i := left(e), right(e)
			if key, ok := i.raw.(string); ok {
				return field(a, key)
			}
			if x, ok := numeric(i.raw); ok && x >= 0 && x < 1e7 && math.Trunc(x) == x {
				return arrayAt(a, int(x))
			}
			return value{}
		}), nil
	case "array":
		constant := true
		raw := make([]any, len(n.args))
		for i, a := range n.args {
			if a.kind != "literal" {
				constant = false
				break
			}
			raw[i] = a.value
		}
		if constant {
			return guarded(func(*env) value { return value{raw: raw} }), nil
		}
		funcs := make([]evalFn, len(n.args))
		for i, a := range n.args {
			f, err := c.compile(a, locals, true, true)
			if err != nil {
				return nil, err
			}
			funcs[i] = f
		}
		return guarded(func(e *env) value {
			items := make([]value, len(funcs))
			for i, f := range funcs {
				items[i] = f(e)
			}
			return value{raw: items}
		}), nil
	case "not":
		f, err := c.compile(n.left, locals, wantFalse, wantTrue)
		if err != nil {
			return nil, err
		}
		return guarded(func(e *env) value {
			b, y, z := f(e).boolean()
			return value{raw: !b, booleanEvidence: true, yes: z, no: y}
		}), nil
	case "binary":
		if n.text == "&&" || n.text == "||" {
			a, err := c.compile(n.left, locals, wantTrue, wantFalse)
			if err != nil {
				return nil, err
			}
			b, err := c.compile(n.right, locals, wantTrue, wantFalse)
			if err != nil {
				return nil, err
			}
			return guarded(func(e *env) value {
				x, xy, xn := a(e).boolean()
				if n.text == "&&" && !x && !wantFalse {
					return boolValue(false, capture{})
				}
				if n.text == "||" && x && !wantTrue {
					return boolValue(true, capture{})
				}
				z, zy, zn := b(e).boolean()
				out := value{booleanEvidence: true}
				if n.text == "&&" {
					out.raw = x && z
					if x && z && wantTrue {
						out.yes = union(xy, zy)
					}
					if !(x && z) && wantFalse {
						if !x {
							out.no = union(out.no, xn)
						}
						if !z {
							out.no = union(out.no, zn)
						}
					}
				} else {
					out.raw = x || z
					if x || z {
						if wantTrue {
							if x {
								out.yes = union(out.yes, xy)
							}
							if z {
								out.yes = union(out.yes, zy)
							}
						}
					} else if wantFalse {
						out.no = union(xn, zn)
					}
				}
				return out
			}), nil
		}
		a, err := c.compile(n.left, locals, true, true)
		if err != nil {
			return nil, err
		}
		b, err := c.compile(n.right, locals, true, true)
		if err != nil {
			return nil, err
		}
		return guarded(func(e *env) value {
			x, y := a(e), b(e)
			return boolValue(compare(n.text, x.raw, y.raw), union(x.effectiveOrigin(), y.effectiveOrigin()))
		}), nil
	case "call":
		return c.call(n, locals, wantTrue, wantFalse)
	case "lambda":
		return c.fail(n, "lambda is only supported by some/every")
	}
	return c.fail(n, "unsupported expression")
}
func (c *compiler) call(n *node, locals map[string]int, wantTrue, wantFalse bool) (evalFn, error) {
	receiver, err := c.compile(n.left, locals, true, true)
	if err != nil {
		return nil, err
	}
	if n.text == "some" || n.text == "every" {
		if len(n.args) != 1 || n.args[0].kind != "lambda" {
			return c.fail(n, "some/every requires one expression lambda")
		}
		lambda := n.args[0]
		if lambda.param == "data" || lambda.param == "context" || lambda.param == "event_type" {
			return c.fail(lambda, "lambda parameter shadows a reserved input")
		}
		scope := make(map[string]int, len(locals)+1)
		for k, v := range locals {
			scope[k] = v
		}
		slot := c.slots
		c.slots++
		scope[lambda.param] = slot
		predicate, err := c.compile(lambda.left, scope, wantTrue, wantFalse)
		if err != nil {
			return nil, err
		}
		return guarded(func(e *env) value {
			a := receiver(e)
			length, ok := arrayLength(a)
			if !ok {
				return boolValue(false, a.effectiveOrigin())
			}
			if length == 0 {
				return boolValue(n.text == "every", a.effectiveOrigin())
			}
			old := e.locals[slot]
			defer func() { e.locals[slot] = old }()
			result := n.text == "every"
			var yes, no capture
			for i := 0; i < length; i++ {
				row := arrayAt(a, i)
				e.locals[slot] = row
				m, y, z := predicate(e).boolean()
				if n.text == "some" {
					if m {
						result = true
						if wantTrue {
							yes = union(yes, union(y, row.effectiveOrigin()))
						}
						if !wantTrue {
							return boolValue(true, capture{})
						}
					} else if wantFalse {
						no = union(no, union(z, row.effectiveOrigin()))
					}
				} else {
					if !m {
						result = false
						if wantFalse {
							no = union(no, union(z, row.effectiveOrigin()))
						}
						if !wantFalse {
							return boolValue(false, capture{})
						}
					} else if wantTrue {
						yes = union(yes, union(y, row.effectiveOrigin()))
					}
				}
				if e.err != nil {
					return value{}
				}
			}
			v := value{raw: result, booleanEvidence: true}
			if result {
				v.yes = yes
			} else {
				v.no = no
			}
			return v
		}), nil
	}
	arity := 1
	if n.text == "toLowerCase" || n.text == "toUpperCase" {
		arity = 0
	}
	switch n.text {
	case "includes", "startsWith", "endsWith", "toLowerCase", "toUpperCase", "matches":
	default:
		return c.fail(n, "unsupported method "+n.text)
	}
	if len(n.args) != arity {
		return c.fail(n, fmt.Sprintf("%s expects %d argument(s)", n.text, arity))
	}
	var argument evalFn
	if arity == 1 {
		argument, err = c.compile(n.args[0], locals, true, true)
		if err != nil {
			return nil, err
		}
	}
	var pattern *regexp.Regexp
	if n.text == "matches" {
		if n.args[0].kind != "literal" {
			return c.fail(n, "matches requires a literal pattern")
		}
		text, ok := n.args[0].value.(string)
		if !ok {
			return c.fail(n, "matches requires a string pattern")
		}
		pattern, err = regexp.Compile(text)
		if err != nil {
			return c.fail(n, "invalid regular expression: "+err.Error())
		}
	}
	return guarded(func(e *env) value {
		a := receiver(e)
		origin := a.effectiveOrigin()
		if arity == 0 {
			s, ok := a.raw.(string)
			if !ok {
				return value{origin: origin}
			}
			if n.text == "toLowerCase" {
				return value{raw: strings.ToLower(s), origin: origin}
			}
			return value{raw: strings.ToUpper(s), origin: origin}
		}
		b := argument(e)
		origin = union(origin, b.effectiveOrigin())
		if n.text == "includes" {
			if size, ok := arrayLength(a); ok {
				found := false
				var selected capture
				for i := 0; i < size; i++ {
					row := arrayAt(a, i)
					if equal(row.raw, b.raw) {
						found = true
						selected = union(selected, row.effectiveOrigin())
					}
				}
				if found {
					return boolValue(true, union(selected, b.effectiveOrigin()))
				}
				return boolValue(false, origin)
			}
		}
		x, ok := a.raw.(string)
		y, yok := b.raw.(string)
		if !ok || !yok {
			return boolValue(false, origin)
		}
		var match bool
		switch n.text {
		case "includes":
			match = strings.Contains(x, y)
		case "startsWith":
			match = strings.HasPrefix(x, y)
		case "endsWith":
			match = strings.HasSuffix(x, y)
		case "matches":
			match = pattern.MatchString(x)
		}
		return boolValue(match, origin)
	}), nil
}
