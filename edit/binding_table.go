package edit

import (
	"errors"
	"sort"

	"github.com/elves/elvish/edit/ui"
	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/eval/types"
	"github.com/elves/elvish/eval/vartypes"
	"github.com/elves/elvish/parse"
)

var errValueShouldBeFn = errors.New("value should be function")

func getBinding(bindingVar vartypes.Variable, k ui.Key) eval.Fn {
	binding := bindingVar.Get().(BindingTable)
	switch {
	case binding.HasKey(k):
		return binding.IndexOne(k).(eval.Fn)
	case binding.HasKey(ui.Default):
		return binding.IndexOne(ui.Default).(eval.Fn)
	default:
		return nil
	}
}

// BindingTable is a special Map that converts its key to ui.Key and ensures
// that its values satisfy eval.CallableValue.
type BindingTable struct {
	types.Map
}

// Repr returns the representation of the binding table as if it were an
// ordinary map keyed by strings.
func (bt BindingTable) Repr(indent int) string {
	var builder types.MapReprBuilder
	builder.Indent = indent

	var keys ui.Keys
	bt.Map.IterateKey(func(k types.Value) bool {
		keys = append(keys, k.(ui.Key))
		return true
	})
	sort.Sort(keys)

	for _, k := range keys {
		v := bt.Map.IndexOne(k)
		builder.WritePair(parse.Quote(k.String()), indent+2, v.Repr(indent+2))
	}

	return builder.String()
}

// IndexOne converts the index to ui.Key and uses the IndexOne of the inner Map.
func (bt BindingTable) IndexOne(idx types.Value) types.Value {
	return bt.Map.IndexOne(ui.ToKey(idx))
}

func (bt BindingTable) get(k ui.Key) eval.Fn {
	return bt.Map.IndexOne(k).(eval.Fn)
}

// Assoc converts the index to ui.Key, ensures that the value is CallableValue,
// uses the Assoc of the inner Map and converts the result to a BindingTable.
func (bt BindingTable) Assoc(k, v types.Value) types.Value {
	key := ui.ToKey(k)
	f, ok := v.(eval.Fn)
	if !ok {
		throw(errValueShouldBeFn)
	}
	return BindingTable{bt.Map.Assoc(key, f).(types.Map)}
}

func makeBindingTable(f *eval.Frame, args []types.Value, opts map[string]types.Value) {
	var raw types.Map
	eval.ScanArgs(args, &raw)
	eval.TakeNoOpt(opts)

	converted := types.EmptyMap
	raw.IteratePair(func(k, v types.Value) bool {
		f, ok := v.(eval.Fn)
		if !ok {
			throw(errValueShouldBeFn)
		}
		converted = converted.Assoc(ui.ToKey(k), f).(types.Map)
		return true
	})

	f.OutputChan() <- BindingTable{converted}
}
