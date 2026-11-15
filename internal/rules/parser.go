package rules

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxSourceBytes = 16 * 1024
const MaxNodes = 2048
const MaxDepth = 64
const MaxEvaluationSteps = 200000

type SyntaxError struct {
	Offset  int
	Message string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("condition byte %d: %s", e.Offset, e.Message)
}

type token struct {
	kind, text string
	pos        int
	literal    any
}
type node struct {
	kind, text  string
	value       any
	left, right *node
	args        []*node
	param       string
	pos         int
}

func lex(source string) ([]token, error) {
	if len(source) > MaxSourceBytes {
		return nil, &SyntaxError{0, "condition exceeds 16 KiB"}
	}
	var out []token
	for i := 0; i < len(source); {
		r, size := utf8.DecodeRuneInString(source[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		start := i
		if unicode.IsLetter(r) || r == '_' || r == '$' {
			i += size
			for i < len(source) {
				rr, sz := utf8.DecodeRuneInString(source[i:])
				if !unicode.IsLetter(rr) && !unicode.IsDigit(rr) && rr != '_' && rr != '$' {
					break
				}
				i += sz
			}
			text := source[start:i]
			t := token{kind: "ident", text: text, pos: start}
			switch text {
			case "true":
				t.kind = "literal"
				t.literal = true
			case "false":
				t.kind = "literal"
				t.literal = false
			case "null", "undefined":
				t.kind = "literal"
			}
			out = append(out, t)
			continue
		}
		if source[i] == '"' || source[i] == '\'' {
			quote := source[i]
			i++
			for i < len(source) {
				if source[i] == '\\' {
					i += 2
					continue
				}
				if source[i] == quote {
					break
				}
				i++
			}
			if i >= len(source) {
				return nil, &SyntaxError{start, "unterminated string"}
			}
			i++
			raw := source[start:i]
			if quote == '\'' {
				var b strings.Builder
				b.WriteByte('"')
				for j := 1; j < len(raw)-1; j++ {
					c := raw[j]
					if c == '\\' && j+1 < len(raw)-1 {
						j++
						if raw[j] == '\'' {
							b.WriteByte('\'')
						} else {
							b.WriteByte('\\')
							b.WriteByte(raw[j])
						}
					} else if c == '"' {
						b.WriteString("\\\"")
					} else {
						b.WriteByte(c)
					}
				}
				b.WriteByte('"')
				raw = b.String()
			}
			var value string
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				return nil, &SyntaxError{start, "invalid string escape"}
			}
			out = append(out, token{kind: "literal", literal: value, pos: start})
			continue
		}
		if source[i] >= '0' && source[i] <= '9' || source[i] == '-' && i+1 < len(source) && source[i+1] >= '0' && source[i+1] <= '9' {
			i++
			for i < len(source) && strings.ContainsRune("0123456789.eE+-", rune(source[i])) {
				i++
			}
			n, err := strconv.ParseFloat(source[start:i], 64)
			if err != nil {
				return nil, &SyntaxError{start, "invalid number"}
			}
			out = append(out, token{kind: "literal", literal: n, pos: start})
			continue
		}
		found := false
		for _, op := range []string{"===", "!==", "=>", "&&", "||", "==", "!=", "<=", ">=", "(", ")", "[", "]", ",", ".", "!", "<", ">"} {
			if strings.HasPrefix(source[i:], op) {
				out = append(out, token{kind: op, text: op, pos: i})
				i += len(op)
				found = true
				break
			}
		}
		if !found {
			return nil, &SyntaxError{start, "unexpected character"}
		}
		if len(out) > MaxNodes*8 {
			return nil, &SyntaxError{start, "condition has too many tokens"}
		}
	}
	out = append(out, token{kind: "eof", pos: len(source)})
	return out, nil
}

type parser struct {
	tokens           []token
	at, nodes, depth int
}

func (p *parser) peek() token { return p.tokens[p.at] }
func (p *parser) accept(kind string) bool {
	if p.peek().kind == kind {
		p.at++
		return true
	}
	return false
}
func (p *parser) expect(kind string) error {
	if !p.accept(kind) {
		return &SyntaxError{p.peek().pos, "expected " + kind}
	}
	return nil
}
func (p *parser) make(n node) (*node, error) {
	p.nodes++
	if p.nodes > MaxNodes {
		return nil, &SyntaxError{n.pos, "condition has too many nodes"}
	}
	return &n, nil
}
func precedence(kind string) int {
	switch kind {
	case "||":
		return 1
	case "&&":
		return 2
	case "==", "!=", "===", "!==":
		return 3
	case "<", "<=", ">", ">=":
		return 4
	}
	return 0
}
func (p *parser) expression(min int) (*node, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > MaxDepth {
		return nil, &SyntaxError{p.peek().pos, "condition nesting exceeds 64"}
	}
	left, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		op := p.peek()
		rank := precedence(op.kind)
		if rank == 0 || rank < min {
			break
		}
		p.at++
		right, err := p.expression(rank + 1)
		if err != nil {
			return nil, err
		}
		left, err = p.make(node{kind: "binary", text: op.kind, left: left, right: right, pos: op.pos})
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}
func (p *parser) argument() (*node, error) {
	t := p.peek()
	if t.kind == "ident" && p.at+1 < len(p.tokens) && p.tokens[p.at+1].kind == "=>" {
		p.at += 2
		body, err := p.expression(1)
		if err != nil {
			return nil, err
		}
		return p.make(node{kind: "lambda", param: t.text, left: body, pos: t.pos})
	}
	if t.kind == "(" && p.at+3 < len(p.tokens) && p.tokens[p.at+1].kind == "ident" && p.tokens[p.at+2].kind == ")" && p.tokens[p.at+3].kind == "=>" {
		name := p.tokens[p.at+1].text
		p.at += 4
		body, err := p.expression(1)
		if err != nil {
			return nil, err
		}
		return p.make(node{kind: "lambda", param: name, left: body, pos: t.pos})
	}
	return p.expression(1)
}
func (p *parser) arguments() ([]*node, error) {
	var args []*node
	if p.accept(")") {
		return args, nil
	}
	for {
		n, err := p.argument()
		if err != nil {
			return nil, err
		}
		args = append(args, n)
		if p.accept(")") {
			break
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
	}
	return args, nil
}
func (p *parser) primary() (*node, error) {
	t := p.peek()
	var n *node
	var err error
	switch {
	case p.accept("!"):
		child, err := p.expression(5)
		if err != nil {
			return nil, err
		}
		return p.make(node{kind: "not", left: child, pos: t.pos})
	case p.accept("("):
		n, err = p.expression(1)
		if err != nil {
			return nil, err
		}
		if err = p.expect(")"); err != nil {
			return nil, err
		}
	case p.accept("["):
		var values []*node
		if !p.accept("]") {
			for {
				v, e := p.expression(1)
				if e != nil {
					return nil, e
				}
				values = append(values, v)
				if p.accept("]") {
					break
				}
				if e = p.expect(","); e != nil {
					return nil, e
				}
			}
		}
		n, err = p.make(node{kind: "array", args: values, pos: t.pos})
	case p.accept("literal"):
		n, err = p.make(node{kind: "literal", value: t.literal, pos: t.pos})
	case p.accept("ident"):
		n, err = p.make(node{kind: "variable", text: t.text, pos: t.pos})
	default:
		return nil, &SyntaxError{t.pos, "expected an expression"}
	}
	if err != nil {
		return nil, err
	}
	for {
		if p.accept(".") {
			field := p.peek()
			if err := p.expect("ident"); err != nil {
				return nil, err
			}
			if p.accept("(") {
				args, e := p.arguments()
				if e != nil {
					return nil, e
				}
				n, err = p.make(node{kind: "call", text: field.text, left: n, args: args, pos: field.pos})
			} else {
				n, err = p.make(node{kind: "property", text: field.text, left: n, pos: field.pos})
			}
		} else if p.accept("[") {
			index, e := p.expression(1)
			if e != nil {
				return nil, e
			}
			if e = p.expect("]"); e != nil {
				return nil, e
			}
			n, err = p.make(node{kind: "index", left: n, right: index, pos: t.pos})
		} else {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return n, nil
}
func parse(source string) (*node, error) {
	tokens, err := lex(source)
	if err != nil {
		return nil, err
	}
	p := parser{tokens: tokens}
	n, err := p.expression(1)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != "eof" {
		return nil, &SyntaxError{p.peek().pos, "unexpected " + p.peek().kind}
	}
	return n, nil
}
