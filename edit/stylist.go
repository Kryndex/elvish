package edit

import (
	"os"

	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/util"
)

type Stylist struct {
	tokens []Token
	editor *Editor
}

func (s *Stylist) applyToTokens(style string, begin, end int) {
	for len(s.tokens) > 0 && s.tokens[0].Node.Begin() < begin {
		// Skip tokens that precede the range
		s.tokens = s.tokens[1:]
	}
	for len(s.tokens) > 0 && s.tokens[0].Node.End() <= end {
		s.tokens[0].addStyle(style)
		s.tokens = s.tokens[1:]
	}
}

func (s *Stylist) style(n parse.Node) {
	if fn, ok := n.(*parse.Form); ok {
		if fn.Head != nil {
			s.formHead(fn.Head)
		}
	}
	for _, child := range n.Children() {
		s.style(child)
	}
}

func (s *Stylist) formHead(n *parse.Compound) {
	simple, head, err := simpleCompound(n, nil)
	st := ""
	if simple {
		if goodFormHead(head, s.editor) {
			st = styleForGoodCommand
		} else {
			st = styleForBadCommand
		}
	} else if err != nil {
		st = styleForBadCommand
	}
	if st != "" {
		s.applyToTokens(st, n.Begin(), n.End())
	}
}

func goodFormHead(head string, ed *Editor) bool {
	if isBuiltinSpecial[head] {
		return true
	} else if util.DontSearch(head) {
		// XXX don't stat twice
		return util.IsExecutable(head) || isDir(head)
	} else {
		_, ns, head := eval.ParseVariable(head)
		if ns == "" {
			return ed.isExternal[head] ||
				eval.Builtin()[eval.FnPrefix+head] != nil ||
				ed.evaler.Global[eval.FnPrefix+head] != nil
		} else if ns == "e" || ns == "E" {
			return ed.isExternal[head]
		} else {
			return ed.evaler.Modules[ns] != nil &&
				ed.evaler.Modules[ns][eval.FnPrefix+head] != nil
		}
	}
}

var isBuiltinSpecial = map[string]bool{}

func init() {
	for _, name := range eval.BuiltinSpecialNames {
		isBuiltinSpecial[name] = true
	}
}

func isDir(fname string) bool {
	stat, err := os.Stat(fname)
	return err == nil && stat.IsDir()
}