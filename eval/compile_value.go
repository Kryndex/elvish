package eval

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/elves/elvish/eval/types"
	"github.com/elves/elvish/glob"
	"github.com/elves/elvish/parse"
	"github.com/xiaq/persistent/hashmap"
)

var outputCaptureBufferSize = 16

// ValuesOp is an operation on an EvalCtx that produce Value's.
type ValuesOp struct {
	Func       ValuesOpFunc
	Begin, End int
}

// ValuesOpFunc is the body of ValuesOp.
type ValuesOpFunc func(*Frame) []types.Value

// Exec executes a ValuesOp and produces Value's.
func (op ValuesOp) Exec(ec *Frame) []types.Value {
	ec.begin, ec.end = op.Begin, op.End
	return op.Func(ec)
}

func (cp *compiler) compound(n *parse.Compound) ValuesOpFunc {
	if len(n.Indexings) == 0 {
		return literalStr("")
	}

	tilde := false
	indexings := n.Indexings

	if n.Indexings[0].Head.Type == parse.Tilde {
		// A lone ~.
		if len(n.Indexings) == 1 {
			return func(ec *Frame) []types.Value {
				return []types.Value{types.String(mustGetHome(""))}
			}
		}
		tilde = true
		indexings = indexings[1:]
	}

	ops := cp.indexingOps(indexings)

	return func(ec *Frame) []types.Value {
		// Accumulator.
		vs := ops[0].Exec(ec)

		// Logger.Printf("concatenating %v with %d more", vs, len(ops)-1)

		for _, op := range ops[1:] {
			us := op.Exec(ec)
			vs = outerProduct(vs, us, cat)
			// Logger.Printf("with %v => %v", us, vs)
		}
		if tilde {
			newvs := make([]types.Value, len(vs))
			for i, v := range vs {
				newvs[i] = doTilde(v)
			}
			vs = newvs
		}
		hasGlob := false
		for _, v := range vs {
			if _, ok := v.(GlobPattern); ok {
				hasGlob = true
				break
			}
		}
		if hasGlob {
			newvs := make([]types.Value, 0, len(vs))
			for _, v := range vs {
				if gp, ok := v.(GlobPattern); ok {
					// Logger.Printf("globbing %v", gp)
					newvs = append(newvs, doGlob(gp, ec.Interrupts())...)
				} else {
					newvs = append(newvs, v)
				}
			}
			vs = newvs
		}
		return vs
	}
}

func cat(lhs, rhs types.Value) types.Value {
	switch lhs := lhs.(type) {
	case types.String:
		switch rhs := rhs.(type) {
		case types.String:
			return lhs + rhs
		case GlobPattern:
			segs := stringToSegments(string(lhs))
			// We know rhs contains exactly one segment.
			segs = append(segs, rhs.Segments[0])
			return GlobPattern{glob.Pattern{segs, ""}, rhs.Flags, rhs.Buts}
		}
	case GlobPattern:
		// NOTE Modifies lhs in place.
		switch rhs := rhs.(type) {
		case types.String:
			lhs.append(stringToSegments(string(rhs))...)
			return lhs
		case GlobPattern:
			// We know rhs contains exactly one segment.
			lhs.append(rhs.Segments[0])
			lhs.Flags |= rhs.Flags
			lhs.Buts = append(lhs.Buts, rhs.Buts...)
			return lhs
		}
	}
	throw(fmt.Errorf("unsupported concat: %s and %s", lhs.Kind(), rhs.Kind()))
	panic("unreachable")
}

func outerProduct(vs []types.Value, us []types.Value, f func(types.Value, types.Value) types.Value) []types.Value {
	ws := make([]types.Value, len(vs)*len(us))
	nu := len(us)
	for i, v := range vs {
		for j, u := range us {
			ws[i*nu+j] = f(v, u)
		}
	}
	return ws
}

// Errors thrown when globbing.
var (
	ErrBadGlobPattern          = errors.New("bad GlobPattern; elvish bug")
	ErrCannotDetermineUsername = errors.New("cannot determine user name from glob pattern")
)

func doTilde(v types.Value) types.Value {
	switch v := v.(type) {
	case types.String:
		s := string(v)
		i := strings.Index(s, "/")
		var uname, rest string
		if i == -1 {
			uname = s
		} else {
			uname = s[:i]
			rest = s[i+1:]
		}
		dir := mustGetHome(uname)
		return types.String(path.Join(dir, rest))
	case GlobPattern:
		if len(v.Segments) == 0 {
			throw(ErrBadGlobPattern)
		}
		switch seg := v.Segments[0].(type) {
		case glob.Literal:
			s := seg.Data
			// Find / in the first segment to determine the username.
			i := strings.Index(s, "/")
			if i == -1 {
				throw(ErrCannotDetermineUsername)
			}
			uname := s[:i]
			dir := mustGetHome(uname)
			// Replace ~uname in first segment with the found path.
			v.Segments[0] = glob.Literal{dir + s[i:]}
		case glob.Slash:
			v.DirOverride = mustGetHome("")
		default:
			throw(ErrCannotDetermineUsername)
		}
		return v
	default:
		throw(fmt.Errorf("tilde doesn't work on value of type %s", v.Kind()))
		panic("unreachable")
	}
}

func (cp *compiler) array(n *parse.Array) ValuesOpFunc {
	return catValuesOps(cp.compoundOps(n.Compounds))
}

func catValuesOps(ops []ValuesOp) ValuesOpFunc {
	return func(ec *Frame) []types.Value {
		// Use number of compound expressions as an estimation of the number
		// of values
		vs := make([]types.Value, 0, len(ops))
		for _, op := range ops {
			us := op.Exec(ec)
			vs = append(vs, us...)
		}
		return vs
	}
}

func (cp *compiler) indexing(n *parse.Indexing) ValuesOpFunc {
	if len(n.Indicies) == 0 {
		return cp.primary(n.Head)
	}

	headOp := cp.primaryOp(n.Head)
	indexOps := cp.arrayOps(n.Indicies)

	return func(ec *Frame) []types.Value {
		vs := headOp.Exec(ec)
		for _, indexOp := range indexOps {
			indicies := indexOp.Exec(ec)
			newvs := make([]types.Value, 0, len(vs)*len(indicies))
			for _, v := range vs {
				newvs = append(newvs, mustIndexer(v, ec).Index(indicies)...)
			}
			vs = newvs
		}
		return vs
	}
}

func literalValues(v ...types.Value) ValuesOpFunc {
	return func(e *Frame) []types.Value {
		return v
	}
}

func literalStr(text string) ValuesOpFunc {
	return literalValues(types.String(text))
}

func variable(qname string) ValuesOpFunc {
	explode, ns, name := ParseVariable(qname)
	return func(ec *Frame) []types.Value {
		variable := ec.ResolveVar(ns, name)
		if variable == nil {
			throwf("variable $%s not found", qname)
		}
		value := variable.Get()
		if explode {
			iterator, ok := value.(types.Iterator)
			if !ok {
				// Use qname[1:] to skip the leading "@"
				throwf("variable $%s (kind %s) cannot be exploded", qname[1:], value.Kind())
			}
			return types.CollectFromIterator(iterator)
		}
		return []types.Value{value}
	}
}

func (cp *compiler) primary(n *parse.Primary) ValuesOpFunc {
	switch n.Type {
	case parse.Bareword, parse.SingleQuoted, parse.DoubleQuoted:
		return literalStr(n.Value)
	case parse.Variable:
		qname := n.Value
		if !cp.registerVariableGetQname(qname) {
			cp.errorf("variable $%s not found", n.Value)
		}
		return variable(qname)
	case parse.Wildcard:
		seg, err := wildcardToSegment(n.SourceText())
		if err != nil {
			cp.errorf("%s", err)
		}
		vs := []types.Value{
			GlobPattern{glob.Pattern{[]glob.Segment{seg}, ""}, 0, nil}}
		return func(ec *Frame) []types.Value {
			return vs
		}
	case parse.Tilde:
		cp.errorf("compiler bug: Tilde not handled in .compound")
		return literalStr("~")
	case parse.ExceptionCapture:
		return cp.exceptionCapture(n.Chunk)
	case parse.OutputCapture:
		return cp.outputCapture(n)
	case parse.List:
		return cp.list(n)
	case parse.Lambda:
		return cp.lambda(n)
	case parse.Map:
		return cp.map_(n)
	case parse.Braced:
		return cp.braced(n)
	default:
		cp.errorf("bad PrimaryType; parser bug")
		return literalStr(n.SourceText())
	}
}

func (cp *compiler) list(n *parse.Primary) ValuesOpFunc {
	// TODO(xiaq): Use Vector.Cons to build the list, instead of building a
	// slice and converting to Vector.
	op := catValuesOps(cp.compoundOps(n.Elements))
	return func(ec *Frame) []types.Value {
		return []types.Value{types.MakeList(op(ec)...)}
	}
}

func (cp *compiler) exceptionCapture(n *parse.Chunk) ValuesOpFunc {
	op := cp.chunkOp(n)
	return func(ec *Frame) []types.Value {
		err := ec.PEval(op)
		if err == nil {
			return []types.Value{OK}
		}
		return []types.Value{err.(*Exception)}
	}
}

func (cp *compiler) outputCapture(n *parse.Primary) ValuesOpFunc {
	op := cp.chunkOp(n.Chunk)
	return func(ec *Frame) []types.Value {
		return captureOutput(ec, op)
	}
}

func captureOutput(ec *Frame, op Op) []types.Value {
	vs, err := pcaptureOutput(ec, op)
	maybeThrow(err)
	return vs
}

func pcaptureOutput(ec *Frame, op Op) ([]types.Value, error) {
	vs := []types.Value{}
	var m sync.Mutex
	valueCb := func(ch <-chan types.Value) {
		for v := range ch {
			m.Lock()
			vs = append(vs, v)
			m.Unlock()
		}
	}
	bytesCb := func(r *os.File) {
		buffered := bufio.NewReader(r)
		for {
			line, err := buffered.ReadString('\n')
			if line != "" {
				v := types.String(strings.TrimSuffix(line, "\n"))
				m.Lock()
				vs = append(vs, v)
				m.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					logger.Println("error on reading:", err)
				}
				break
			}
		}
	}

	err := pcaptureOutputInner(ec, op, valueCb, bytesCb)
	return vs, err
}

func pcaptureOutputInner(ec *Frame, op Op, valuesCb func(<-chan types.Value), bytesCb func(*os.File)) error {

	newEc := ec.fork("[output capture]")

	ch := make(chan types.Value, outputCaptureBufferSize)
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %v", err)
	}
	newEc.ports[1] = &Port{
		Chan: ch, CloseChan: true,
		File: pipeWrite, CloseFile: true,
	}

	bytesCollected := make(chan struct{})
	chCollected := make(chan struct{})

	go func() {
		valuesCb(ch)
		close(chCollected)
	}()
	go func() {
		bytesCb(pipeRead)
		pipeRead.Close()
		close(bytesCollected)
	}()

	err = newEc.PEval(op)

	ClosePorts(newEc.ports)
	<-bytesCollected
	<-chCollected

	return err
}

func (cp *compiler) lambda(n *parse.Primary) ValuesOpFunc {
	// Parse signature.
	var (
		argNames      []string
		restArgName   string
		optNames      []string
		optDefaultOps []ValuesOp
	)
	if len(n.Elements) > 0 {
		// Argument list.
		argNames = make([]string, len(n.Elements))
		for i, arg := range n.Elements {
			qname := mustString(cp, arg, "argument name must be literal string")
			explode, ns, name := ParseVariable(qname)
			if ns != "" {
				cp.errorpf(arg.Begin(), arg.End(), "argument name must be unqualified")
			}
			if name == "" {
				cp.errorpf(arg.Begin(), arg.End(), "argument name must not be empty")
			}
			if explode {
				if i != len(n.Elements)-1 {
					cp.errorpf(arg.Begin(), arg.End(), "only the last argument may have @")
				}
				restArgName = name
				argNames = argNames[:i]
			} else {
				argNames[i] = name
			}
		}
	}
	if len(n.MapPairs) > 0 {
		optNames = make([]string, len(n.MapPairs))
		optDefaultOps = make([]ValuesOp, len(n.MapPairs))
		for i, opt := range n.MapPairs {
			qname := mustString(cp, opt.Key, "option name must be literal string")
			_, ns, name := ParseVariable(qname)
			if ns != "" {
				cp.errorpf(opt.Key.Begin(), opt.Key.End(), "option name must be unqualified")
			}
			if name == "" {
				cp.errorpf(opt.Key.Begin(), opt.Key.End(), "option name must not be empty")
			}
			optNames[i] = name
			if opt.Value == nil {
				cp.errorpf(opt.End(), opt.End(), "option must have default value")
			} else {
				optDefaultOps[i] = cp.compoundOp(opt.Value)
			}
		}
	}

	thisScope := cp.pushScope()
	for _, argName := range argNames {
		thisScope.set(argName)
	}
	if restArgName != "" {
		thisScope.set(restArgName)
	}
	for _, optName := range optNames {
		thisScope.set(optName)
	}
	thisScope.set("opts")

	op := cp.chunkOp(n.Chunk)

	// XXX The fiddlings with cp.capture is error-prone.
	capture := cp.capture
	cp.capture = make(staticNs)
	cp.popScope()

	for name := range capture {
		cp.registerVariableGetQname(name)
	}

	srcMeta := cp.srcMeta

	return func(ec *Frame) []types.Value {
		evCapture := make(Ns)
		for name := range capture {
			evCapture[name] = ec.ResolveVar("", name)
		}
		optDefaults := make([]types.Value, len(optDefaultOps))
		for i, op := range optDefaultOps {
			defaultValue := ec.ExecAndUnwrap("option default value", op).One().Any()
			optDefaults[i] = defaultValue
		}
		// XXX(xiaq): Capture uses.
		return []types.Value{&Closure{argNames, restArgName, optNames, optDefaults, op, evCapture, srcMeta}}
	}
}

func (cp *compiler) map_(n *parse.Primary) ValuesOpFunc {
	return cp.mapPairs(n.MapPairs)
}

func (cp *compiler) mapPairs(pairs []*parse.MapPair) ValuesOpFunc {
	npairs := len(pairs)
	keysOps := make([]ValuesOp, npairs)
	valuesOps := make([]ValuesOp, npairs)
	begins, ends := make([]int, npairs), make([]int, npairs)
	for i, pair := range pairs {
		keysOps[i] = cp.compoundOp(pair.Key)
		if pair.Value == nil {
			p := pair.End()
			valuesOps[i] = ValuesOp{literalValues(types.Bool(true)), p, p}
		} else {
			valuesOps[i] = cp.compoundOp(pairs[i].Value)
		}
		begins[i], ends[i] = pair.Begin(), pair.End()
	}
	return func(ec *Frame) []types.Value {
		m := hashmap.Empty
		for i := 0; i < npairs; i++ {
			keys := keysOps[i].Exec(ec)
			values := valuesOps[i].Exec(ec)
			if len(keys) != len(values) {
				ec.errorpf(begins[i], ends[i],
					"%d keys but %d values", len(keys), len(values))
			}
			for j, key := range keys {
				m = m.Assoc(key, values[j])
			}
		}
		return []types.Value{types.NewMap(m)}
	}
}

func (cp *compiler) braced(n *parse.Primary) ValuesOpFunc {
	ops := cp.compoundOps(n.Braced)
	// TODO: n.IsRange
	// isRange := n.IsRange
	return catValuesOps(ops)
}
