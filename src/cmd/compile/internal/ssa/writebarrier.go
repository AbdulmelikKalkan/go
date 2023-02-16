// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
	"fmt"
	"internal/buildcfg"
)

// A ZeroRegion records parts of an object which are known to be zero.
// A ZeroRegion only applies to a single memory state.
// Each bit in mask is set if the corresponding pointer-sized word of
// the base object is known to be zero.
// In other words, if mask & (1<<i) != 0, then [base+i*ptrSize, base+(i+1)*ptrSize)
// is known to be zero.
type ZeroRegion struct {
	base *Value
	mask uint64
}

// mightBeHeapPointer reports whether v might point to the heap.
// v must have pointer type.
func mightBeHeapPointer(v *Value) bool {
	if IsGlobalAddr(v) {
		return false
	}
	return true
}

// mightContainHeapPointer reports whether the data currently at addresses
// [ptr,ptr+size) might contain heap pointers. "currently" means at memory state mem.
// zeroes contains ZeroRegion data to help make that decision (see computeZeroMap).
func mightContainHeapPointer(ptr *Value, size int64, mem *Value, zeroes map[ID]ZeroRegion) bool {
	if IsReadOnlyGlobalAddr(ptr) {
		// The read-only globals section cannot contain any heap pointers.
		return false
	}

	// See if we can prove that the queried memory is all zero.

	// Find base pointer and offset. Hopefully, the base is the result of a new(T).
	var off int64
	for ptr.Op == OpOffPtr {
		off += ptr.AuxInt
		ptr = ptr.Args[0]
	}

	ptrSize := ptr.Block.Func.Config.PtrSize
	if off%ptrSize != 0 || size%ptrSize != 0 {
		ptr.Fatalf("unaligned pointer write")
	}
	if off < 0 || off+size > 64*ptrSize {
		// memory range goes off end of tracked offsets
		return true
	}
	z := zeroes[mem.ID]
	if ptr != z.base {
		// This isn't the object we know about at this memory state.
		return true
	}
	// Mask of bits we're asking about
	m := (uint64(1)<<(size/ptrSize) - 1) << (off / ptrSize)

	if z.mask&m == m {
		// All locations are known to be zero, so no heap pointers.
		return false
	}
	return true
}

// needwb reports whether we need write barrier for store op v.
// v must be Store/Move/Zero.
// zeroes provides known zero information (keyed by ID of memory-type values).
func needwb(v *Value, zeroes map[ID]ZeroRegion) bool {
	t, ok := v.Aux.(*types.Type)
	if !ok {
		v.Fatalf("store aux is not a type: %s", v.LongString())
	}
	if !t.HasPointers() {
		return false
	}
	dst := v.Args[0]
	if IsStackAddr(dst) {
		return false // writes into the stack don't need write barrier
	}
	// If we're writing to a place that might have heap pointers, we need
	// the write barrier.
	if mightContainHeapPointer(dst, t.Size(), v.MemoryArg(), zeroes) {
		return true
	}
	// Lastly, check if the values we're writing might be heap pointers.
	// If they aren't, we don't need a write barrier.
	switch v.Op {
	case OpStore:
		if !mightBeHeapPointer(v.Args[1]) {
			return false
		}
	case OpZero:
		return false // nil is not a heap pointer
	case OpMove:
		if !mightContainHeapPointer(v.Args[1], t.Size(), v.Args[2], zeroes) {
			return false
		}
	default:
		v.Fatalf("store op unknown: %s", v.LongString())
	}
	return true
}

// writebarrier pass inserts write barriers for store ops (Store, Move, Zero)
// when necessary (the condition above). It rewrites store ops to branches
// and runtime calls, like
//
//	if writeBarrier.enabled {
//		gcWriteBarrier(ptr, val)	// Not a regular Go call
//	} else {
//		*ptr = val
//	}
//
// A sequence of WB stores for many pointer fields of a single type will
// be emitted together, with a single branch.
func writebarrier(f *Func) {
	if !f.fe.UseWriteBarrier() {
		return
	}

	var sb, sp, wbaddr, const0 *Value
	var typedmemmove, typedmemclr, gcWriteBarrier, cgoCheckPtrWrite, cgoCheckMemmove *obj.LSym
	var stores, after []*Value
	var sset *sparseSet
	var storeNumber []int32

	// Compute map from a value to the SelectN [1] value that uses it.
	select1 := f.Cache.allocValueSlice(f.NumValues())
	defer func() { f.Cache.freeValueSlice(select1) }()
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if v.Op != OpSelectN {
				continue
			}
			if v.AuxInt != 1 {
				continue
			}
			select1[v.Args[0].ID] = v
		}
	}

	zeroes := f.computeZeroMap(select1)
	for _, b := range f.Blocks { // range loop is safe since the blocks we added contain no stores to expand
		// first, identify all the stores that need to insert a write barrier.
		// mark them with WB ops temporarily. record presence of WB ops.
		nWBops := 0 // count of temporarily created WB ops remaining to be rewritten in the current block
		for _, v := range b.Values {
			switch v.Op {
			case OpStore, OpMove, OpZero:
				if needwb(v, zeroes) {
					switch v.Op {
					case OpStore:
						v.Op = OpStoreWB
					case OpMove:
						v.Op = OpMoveWB
					case OpZero:
						v.Op = OpZeroWB
					}
					nWBops++
				}
			}
		}
		if nWBops == 0 {
			continue
		}

		if wbaddr == nil {
			// lazily initialize global values for write barrier test and calls
			// find SB and SP values in entry block
			initpos := f.Entry.Pos
			sp, sb = f.spSb()
			wbsym := f.fe.Syslook("writeBarrier")
			wbaddr = f.Entry.NewValue1A(initpos, OpAddr, f.Config.Types.UInt32Ptr, wbsym, sb)
			gcWriteBarrier = f.fe.Syslook("gcWriteBarrier")
			typedmemmove = f.fe.Syslook("typedmemmove")
			typedmemclr = f.fe.Syslook("typedmemclr")
			if buildcfg.Experiment.CgoCheck2 {
				cgoCheckPtrWrite = f.fe.Syslook("cgoCheckPtrWrite")
				cgoCheckMemmove = f.fe.Syslook("cgoCheckMemmove")
			}
			const0 = f.ConstInt32(f.Config.Types.UInt32, 0)

			// allocate auxiliary data structures for computing store order
			sset = f.newSparseSet(f.NumValues())
			defer f.retSparseSet(sset)
			storeNumber = f.Cache.allocInt32Slice(f.NumValues())
			defer f.Cache.freeInt32Slice(storeNumber)
		}

		// order values in store order
		b.Values = storeOrder(b.Values, sset, storeNumber)
	again:
		// find the start and end of the last contiguous WB store sequence.
		// a branch will be inserted there. values after it will be moved
		// to a new block.
		var last *Value
		var start, end int
		values := b.Values
	FindSeq:
		for i := len(values) - 1; i >= 0; i-- {
			w := values[i]
			switch w.Op {
			case OpStoreWB, OpMoveWB, OpZeroWB:
				start = i
				if last == nil {
					last = w
					end = i + 1
				}
			case OpVarDef, OpVarLive:
				continue
			default:
				if last == nil {
					continue
				}
				break FindSeq
			}
		}
		stores = append(stores[:0], b.Values[start:end]...) // copy to avoid aliasing
		after = append(after[:0], b.Values[end:]...)
		b.Values = b.Values[:start]

		// find the memory before the WB stores
		mem := stores[0].MemoryArg()
		pos := stores[0].Pos
		bThen := f.NewBlock(BlockPlain)
		bElse := f.NewBlock(BlockPlain)
		bEnd := f.NewBlock(b.Kind)
		bThen.Pos = pos
		bElse.Pos = pos
		bEnd.Pos = b.Pos
		b.Pos = pos

		// set up control flow for end block
		bEnd.CopyControls(b)
		bEnd.Likely = b.Likely
		for _, e := range b.Succs {
			bEnd.Succs = append(bEnd.Succs, e)
			e.b.Preds[e.i].b = bEnd
		}

		// set up control flow for write barrier test
		// load word, test word, avoiding partial register write from load byte.
		cfgtypes := &f.Config.Types
		flag := b.NewValue2(pos, OpLoad, cfgtypes.UInt32, wbaddr, mem)
		flag = b.NewValue2(pos, OpNeq32, cfgtypes.Bool, flag, const0)
		b.Kind = BlockIf
		b.SetControl(flag)
		b.Likely = BranchUnlikely
		b.Succs = b.Succs[:0]
		b.AddEdgeTo(bThen)
		b.AddEdgeTo(bElse)
		// TODO: For OpStoreWB and the buffered write barrier,
		// we could move the write out of the write barrier,
		// which would lead to fewer branches. We could do
		// something similar to OpZeroWB, since the runtime
		// could provide just the barrier half and then we
		// could unconditionally do an OpZero (which could
		// also generate better zeroing code). OpMoveWB is
		// trickier and would require changing how
		// cgoCheckMemmove works.
		bThen.AddEdgeTo(bEnd)
		bElse.AddEdgeTo(bEnd)

		// for each write barrier store, append write barrier version to bThen
		// and simple store version to bElse
		memThen := mem
		memElse := mem

		// If the source of a MoveWB is volatile (will be clobbered by a
		// function call), we need to copy it to a temporary location, as
		// marshaling the args of typedmemmove might clobber the value we're
		// trying to move.
		// Look for volatile source, copy it to temporary before we emit any
		// call.
		// It is unlikely to have more than one of them. Just do a linear
		// search instead of using a map.
		type volatileCopy struct {
			src *Value // address of original volatile value
			tmp *Value // address of temporary we've copied the volatile value into
		}
		var volatiles []volatileCopy
	copyLoop:
		for _, w := range stores {
			if w.Op == OpMoveWB {
				val := w.Args[1]
				if isVolatile(val) {
					for _, c := range volatiles {
						if val == c.src {
							continue copyLoop // already copied
						}
					}

					t := val.Type.Elem()
					tmp := f.fe.Auto(w.Pos, t)
					memThen = bThen.NewValue1A(w.Pos, OpVarDef, types.TypeMem, tmp, memThen)
					tmpaddr := bThen.NewValue2A(w.Pos, OpLocalAddr, t.PtrTo(), tmp, sp, memThen)
					siz := t.Size()
					memThen = bThen.NewValue3I(w.Pos, OpMove, types.TypeMem, siz, tmpaddr, val, memThen)
					memThen.Aux = t
					volatiles = append(volatiles, volatileCopy{val, tmpaddr})
				}
			}
		}

		for _, w := range stores {
			ptr := w.Args[0]
			pos := w.Pos

			var fn *obj.LSym
			var typ *obj.LSym
			var val *Value
			switch w.Op {
			case OpStoreWB:
				val = w.Args[1]
				nWBops--
			case OpMoveWB:
				fn = typedmemmove
				val = w.Args[1]
				typ = reflectdata.TypeLinksym(w.Aux.(*types.Type))
				nWBops--
			case OpZeroWB:
				fn = typedmemclr
				typ = reflectdata.TypeLinksym(w.Aux.(*types.Type))
				nWBops--
			case OpVarDef, OpVarLive:
			}

			// then block: emit write barrier call
			switch w.Op {
			case OpStoreWB, OpMoveWB, OpZeroWB:
				if w.Op == OpStoreWB {
					if buildcfg.Experiment.CgoCheck2 {
						// Issue cgo checking code.
						memThen = wbcall(pos, bThen, cgoCheckPtrWrite, nil, ptr, val, memThen, sp, sb)
					}

					memThen = bThen.NewValue3A(pos, OpWB, types.TypeMem, gcWriteBarrier, ptr, val, memThen)
				} else {
					srcval := val
					if w.Op == OpMoveWB && isVolatile(srcval) {
						for _, c := range volatiles {
							if srcval == c.src {
								srcval = c.tmp
								break
							}
						}
					}
					memThen = wbcall(pos, bThen, fn, typ, ptr, srcval, memThen, sp, sb)
				}
				// Note that we set up a writebarrier function call.
				f.fe.SetWBPos(pos)
			case OpVarDef, OpVarLive:
				memThen = bThen.NewValue1A(pos, w.Op, types.TypeMem, w.Aux, memThen)
			}

			// else block: normal store
			switch w.Op {
			case OpStoreWB:
				if buildcfg.Experiment.CgoCheck2 {
					// Issue cgo checking code.
					memElse = wbcall(pos, bElse, cgoCheckPtrWrite, nil, ptr, val, memElse, sp, sb)
				}
				memElse = bElse.NewValue3A(pos, OpStore, types.TypeMem, w.Aux, ptr, val, memElse)
			case OpMoveWB:
				if buildcfg.Experiment.CgoCheck2 {
					// Issue cgo checking code.
					memElse = wbcall(pos, bElse, cgoCheckMemmove, reflectdata.TypeLinksym(w.Aux.(*types.Type)), ptr, val, memElse, sp, sb)
				}
				memElse = bElse.NewValue3I(pos, OpMove, types.TypeMem, w.AuxInt, ptr, val, memElse)
				memElse.Aux = w.Aux
			case OpZeroWB:
				memElse = bElse.NewValue2I(pos, OpZero, types.TypeMem, w.AuxInt, ptr, memElse)
				memElse.Aux = w.Aux
			case OpVarDef, OpVarLive:
				memElse = bElse.NewValue1A(pos, w.Op, types.TypeMem, w.Aux, memElse)
			}
		}

		// merge memory
		mem = bEnd.NewValue2(pos, OpPhi, types.TypeMem, memThen, memElse)
		// The last store becomes the WBend marker. This marker is used by the liveness
		// pass to determine what parts of the code are preemption-unsafe.
		// All subsequent memory operations use this memory, so we have to sacrifice the
		// previous last memory op to become this new value.
		bEnd.Values = append(bEnd.Values, last)
		last.Block = bEnd
		last.reset(OpWBend)
		last.Pos = last.Pos.WithNotStmt()
		last.Type = types.TypeMem
		last.AddArg(mem)

		// Free all the old stores, except last which became the WBend marker.
		for _, w := range stores {
			if w != last {
				w.resetArgs()
			}
		}
		for _, w := range stores {
			if w != last {
				f.freeValue(w)
			}
		}

		// put values after the store sequence into the end block
		bEnd.Values = append(bEnd.Values, after...)
		for _, w := range after {
			w.Block = bEnd
		}

		// if we have more stores in this block, do this block again
		if nWBops > 0 {
			goto again
		}
	}
}

// computeZeroMap returns a map from an ID of a memory value to
// a set of locations that are known to be zeroed at that memory value.
func (f *Func) computeZeroMap(select1 []*Value) map[ID]ZeroRegion {

	ptrSize := f.Config.PtrSize
	// Keep track of which parts of memory are known to be zero.
	// This helps with removing write barriers for various initialization patterns.
	// This analysis is conservative. We only keep track, for each memory state, of
	// which of the first 64 words of a single object are known to be zero.
	zeroes := map[ID]ZeroRegion{}
	// Find new objects.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if mem, ok := IsNewObject(v, select1); ok {
				// While compiling package runtime itself, we might see user
				// calls to newobject, which will have result type
				// unsafe.Pointer instead. We can't easily infer how large the
				// allocated memory is, so just skip it.
				if types.LocalPkg.Path == "runtime" && v.Type.IsUnsafePtr() {
					continue
				}

				nptr := v.Type.Elem().Size() / ptrSize
				if nptr > 64 {
					nptr = 64
				}
				zeroes[mem.ID] = ZeroRegion{base: v, mask: 1<<uint(nptr) - 1}
			}
		}
	}
	// Find stores to those new objects.
	for {
		changed := false
		for _, b := range f.Blocks {
			// Note: iterating forwards helps convergence, as values are
			// typically (but not always!) in store order.
			for _, v := range b.Values {
				if v.Op != OpStore {
					continue
				}
				z, ok := zeroes[v.MemoryArg().ID]
				if !ok {
					continue
				}
				ptr := v.Args[0]
				var off int64
				size := v.Aux.(*types.Type).Size()
				for ptr.Op == OpOffPtr {
					off += ptr.AuxInt
					ptr = ptr.Args[0]
				}
				if ptr != z.base {
					// Different base object - we don't know anything.
					// We could even be writing to the base object we know
					// about, but through an aliased but offset pointer.
					// So we have to throw all the zero information we have away.
					continue
				}
				// Round to cover any partially written pointer slots.
				// Pointer writes should never be unaligned like this, but non-pointer
				// writes to pointer-containing types will do this.
				if d := off % ptrSize; d != 0 {
					off -= d
					size += d
				}
				if d := size % ptrSize; d != 0 {
					size += ptrSize - d
				}
				// Clip to the 64 words that we track.
				min := off
				max := off + size
				if min < 0 {
					min = 0
				}
				if max > 64*ptrSize {
					max = 64 * ptrSize
				}
				// Clear bits for parts that we are writing (and hence
				// will no longer necessarily be zero).
				for i := min; i < max; i += ptrSize {
					bit := i / ptrSize
					z.mask &^= 1 << uint(bit)
				}
				if z.mask == 0 {
					// No more known zeros - don't bother keeping.
					continue
				}
				// Save updated known zero contents for new store.
				if zeroes[v.ID] != z {
					zeroes[v.ID] = z
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	if f.pass.debug > 0 {
		fmt.Printf("func %s\n", f.Name)
		for mem, z := range zeroes {
			fmt.Printf("  memory=v%d ptr=%v zeromask=%b\n", mem, z.base, z.mask)
		}
	}
	return zeroes
}

// wbcall emits write barrier runtime call in b, returns memory.
func wbcall(pos src.XPos, b *Block, fn, typ *obj.LSym, ptr, val, mem, sp, sb *Value) *Value {
	config := b.Func.Config

	var wbargs []*Value
	// TODO (register args) this is a bit of a hack.
	inRegs := b.Func.ABIDefault == b.Func.ABI1 && len(config.intParamRegs) >= 3

	// put arguments on stack
	off := config.ctxt.Arch.FixedFrameSize

	var argTypes []*types.Type
	if typ != nil { // for typedmemmove/cgoCheckMemmove
		taddr := b.NewValue1A(pos, OpAddr, b.Func.Config.Types.Uintptr, typ, sb)
		argTypes = append(argTypes, b.Func.Config.Types.Uintptr)
		off = round(off, taddr.Type.Alignment())
		if inRegs {
			wbargs = append(wbargs, taddr)
		} else {
			arg := b.NewValue1I(pos, OpOffPtr, taddr.Type.PtrTo(), off, sp)
			mem = b.NewValue3A(pos, OpStore, types.TypeMem, ptr.Type, arg, taddr, mem)
		}
		off += taddr.Type.Size()
	}

	argTypes = append(argTypes, ptr.Type)
	off = round(off, ptr.Type.Alignment())
	if inRegs {
		wbargs = append(wbargs, ptr)
	} else {
		arg := b.NewValue1I(pos, OpOffPtr, ptr.Type.PtrTo(), off, sp)
		mem = b.NewValue3A(pos, OpStore, types.TypeMem, ptr.Type, arg, ptr, mem)
	}
	off += ptr.Type.Size()

	if val != nil {
		argTypes = append(argTypes, val.Type)
		off = round(off, val.Type.Alignment())
		if inRegs {
			wbargs = append(wbargs, val)
		} else {
			arg := b.NewValue1I(pos, OpOffPtr, val.Type.PtrTo(), off, sp)
			mem = b.NewValue3A(pos, OpStore, types.TypeMem, val.Type, arg, val, mem)
		}
		off += val.Type.Size()
	}
	off = round(off, config.PtrSize)
	wbargs = append(wbargs, mem)

	// issue call
	call := b.NewValue0A(pos, OpStaticCall, types.TypeResultMem, StaticAuxCall(fn, b.Func.ABIDefault.ABIAnalyzeTypes(nil, argTypes, nil)))
	call.AddArgs(wbargs...)
	call.AuxInt = off - config.ctxt.Arch.FixedFrameSize
	return b.NewValue1I(pos, OpSelectN, types.TypeMem, 0, call)
}

// round to a multiple of r, r is a power of 2.
func round(o int64, r int64) int64 {
	return (o + r - 1) &^ (r - 1)
}

// IsStackAddr reports whether v is known to be an address of a stack slot.
func IsStackAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	switch v.Op {
	case OpSP, OpLocalAddr, OpSelectNAddr, OpGetCallerSP:
		return true
	}
	return false
}

// IsGlobalAddr reports whether v is known to be an address of a global (or nil).
func IsGlobalAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	if v.Op == OpAddr && v.Args[0].Op == OpSB {
		return true // address of a global
	}
	if v.Op == OpConstNil {
		return true
	}
	if v.Op == OpLoad && IsReadOnlyGlobalAddr(v.Args[0]) {
		return true // loading from a read-only global - the resulting address can't be a heap address.
	}
	return false
}

// IsReadOnlyGlobalAddr reports whether v is known to be an address of a read-only global.
func IsReadOnlyGlobalAddr(v *Value) bool {
	if v.Op == OpConstNil {
		// Nil pointers are read only. See issue 33438.
		return true
	}
	if v.Op == OpAddr && v.Aux != nil && v.Aux.(*obj.LSym).Type == objabi.SRODATA {
		return true
	}
	return false
}

// IsNewObject reports whether v is a pointer to a freshly allocated & zeroed object,
// if so, also returns the memory state mem at which v is zero.
func IsNewObject(v *Value, select1 []*Value) (mem *Value, ok bool) {
	f := v.Block.Func
	c := f.Config
	if f.ABIDefault == f.ABI1 && len(c.intParamRegs) >= 1 {
		if v.Op != OpSelectN || v.AuxInt != 0 {
			return nil, false
		}
		mem = select1[v.Args[0].ID]
		if mem == nil {
			return nil, false
		}
	} else {
		if v.Op != OpLoad {
			return nil, false
		}
		mem = v.MemoryArg()
		if mem.Op != OpSelectN {
			return nil, false
		}
		if mem.Type != types.TypeMem {
			return nil, false
		} // assume it is the right selection if true
	}
	call := mem.Args[0]
	if call.Op != OpStaticCall {
		return nil, false
	}
	if !isSameCall(call.Aux, "runtime.newobject") {
		return nil, false
	}
	if f.ABIDefault == f.ABI1 && len(c.intParamRegs) >= 1 {
		if v.Args[0] == call {
			return mem, true
		}
		return nil, false
	}
	if v.Args[0].Op != OpOffPtr {
		return nil, false
	}
	if v.Args[0].Args[0].Op != OpSP {
		return nil, false
	}
	if v.Args[0].AuxInt != c.ctxt.Arch.FixedFrameSize+c.RegSize { // offset of return value
		return nil, false
	}
	return mem, true
}

// IsSanitizerSafeAddr reports whether v is known to be an address
// that doesn't need instrumentation.
func IsSanitizerSafeAddr(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy {
		v = v.Args[0]
	}
	switch v.Op {
	case OpSP, OpLocalAddr, OpSelectNAddr:
		// Stack addresses are always safe.
		return true
	case OpITab, OpStringPtr, OpGetClosurePtr:
		// Itabs, string data, and closure fields are
		// read-only once initialized.
		return true
	case OpAddr:
		vt := v.Aux.(*obj.LSym).Type
		return vt == objabi.SRODATA || vt == objabi.SLIBFUZZER_8BIT_COUNTER || vt == objabi.SCOVERAGE_COUNTER || vt == objabi.SCOVERAGE_AUXVAR
	}
	return false
}

// isVolatile reports whether v is a pointer to argument region on stack which
// will be clobbered by a function call.
func isVolatile(v *Value) bool {
	for v.Op == OpOffPtr || v.Op == OpAddPtr || v.Op == OpPtrIndex || v.Op == OpCopy || v.Op == OpSelectNAddr {
		v = v.Args[0]
	}
	return v.Op == OpSP
}
