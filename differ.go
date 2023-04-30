package jsondiff

import (
	"sort"
	"strings"
)

// A Differ is a JSON Patch generator.
// The zero value is an empty generator ready to use.
type Differ struct {
	patch       Patch
	hasher      hasher
	hashmap     map[uint64]jsonNode
	targetBytes []byte
	opts        options
	ptr         pointer
}

type (
	marshalFunc   func(any) ([]byte, error)
	unmarshalFunc func([]byte, any) error
)

type options struct {
	factorize   bool
	rationalize bool
	invertible  bool
	equivalent  bool
	ignores     map[string]struct{}
	marshal     marshalFunc
	unmarshal   unmarshalFunc
}

type jsonNode struct {
	ptr string
	val any
}

// Patch returns the list of JSON patch operations
// generated by the Differ. The patch is valid for usage
// until the next comparison or reset.
func (d *Differ) Patch() Patch {
	return d.patch
}

// WithOpts applies the given options to the Differ
// and returns it to allow chained calls.
func (d *Differ) WithOpts(opts ...Option) *Differ {
	for _, o := range opts {
		o(d)
	}
	return d
}

// Reset resets the Differ to be empty, but it retains the
// underlying storage for use by future comparisons.
func (d *Differ) Reset() {
	d.patch = d.patch[:0]
	d.ptr = d.ptr.reset()

	// Optimized map clear.
	for k := range d.hashmap {
		delete(d.hashmap, k)
	}
}

// Compare computes the differences between src and tgt as
// a series of JSON Patch operations.
func (d *Differ) Compare(src, tgt interface{}) {
	if d.opts.factorize {
		d.prepare(d.ptr, src, tgt)
	}
	d.ptr.reset()
	d.diff(d.ptr, src, tgt)
}

func (d *Differ) diff(ptr pointer, src, tgt interface{}) {
	if src == nil && tgt == nil {
		return
	}
	if _, ok := d.opts.ignores[ptr.copy()]; ok {
		return
	}
	if !areComparable(src, tgt) {
		if ptr.isRoot() {
			// If incomparable values are located at the root
			// of the document, use an add operation to replace
			// the entire content of the document.
			// https://tools.ietf.org/html/rfc6902#section-4.1
			d.patch = d.patch.append(OperationAdd, emptyPtr, ptr.copy(), src, tgt)
		} else {
			// Values are incomparable, generate a replacement.
			d.replace(ptr.copy(), src, tgt)
		}
		return
	}
	if deepValueEqual(src, tgt, typeSwitchKind(src)) {
		return
	}
	size := len(d.patch)

	// Values are comparable, but are not
	// equivalent.
	switch val := src.(type) {
	case []interface{}:
		d.compareArrays(ptr, val, tgt.([]interface{}))
	case map[string]interface{}:
		d.compareObjects(ptr, val, tgt.(map[string]interface{}))
	default:
		// Generate a replace operation for
		// scalar types.
		if !deepValueEqual(src, tgt, typeSwitchKind(src)) {
			d.replace(ptr.copy(), src, tgt)
			return
		}
	}
	// Rationalize any new operations.
	if d.opts.rationalize && len(d.patch) > size {
		d.rationalizeLastOps(ptr, src, tgt, size)
	}
}

func (d *Differ) prepare(ptr pointer, src, tgt interface{}) {
	if src == nil && tgt == nil {
		return
	}
	// When both values are deeply equals, save
	// the location indexed by the value hash.
	if !areComparable(src, tgt) {
		return
	} else if deepValueEqual(src, tgt, typeSwitchKind(src)) {
		k := d.hasher.digest(tgt)
		if d.hashmap == nil {
			d.hashmap = make(map[uint64]jsonNode)
		}
		d.hashmap[k] = jsonNode{
			ptr: ptr.copy(),
			val: tgt,
		}
		return
	}
	// At this point, the source and target values
	// are non-nil and have comparable types.
	switch vsrc := src.(type) {
	case []interface{}:
		oarr := vsrc
		narr := tgt.([]interface{})

		for i := 0; i < min(len(oarr), len(narr)); i++ {
			d.prepare(ptr.appendIndex(i), oarr[i], narr[i])
		}
	case map[string]interface{}:
		oobj := vsrc
		nobj := tgt.(map[string]interface{})

		for k, v1 := range oobj {
			if v2, ok := nobj[k]; ok {
				d.prepare(ptr.appendKey(k), v1, v2)
			}
		}
	default:
		// Skipped.
	}
}

func (d *Differ) rationalizeLastOps(ptr pointer, src, tgt interface{}, lastOpIdx int) {
	newOps := make(Patch, 0, 2)

	if d.opts.invertible {
		newOps = newOps.append(OperationTest, emptyPtr, ptr.copy(), nil, src)
	}
	// replaceOp represents a single operation that
	// replace the source document with the target.
	replaceOp := Operation{
		Type:  OperationReplace,
		Path:  ptr.copy(),
		Value: tgt,
	}
	newOps = append(newOps, replaceOp)
	curOps := d.patch[lastOpIdx:]

	newLen := replaceOp.jsonLength(d.targetBytes)
	curLen := curOps.jsonLength(d.targetBytes)

	// If one operation is cheaper than many small
	// operations that represents the changes between
	// the two objects, replace the last operations.
	if curLen > newLen {
		d.patch = d.patch[:lastOpIdx]
		d.patch = append(d.patch, newOps...)
	}
}

// compareObjects generates the patch operations that
// represents the differences between two JSON objects.
func (d *Differ) compareObjects(ptr pointer, src, tgt map[string]interface{}) {
	cmpSet := map[string]uint8{}

	for k := range src {
		cmpSet[k] |= 1 << 0
	}
	for k := range tgt {
		cmpSet[k] |= 1 << 1
	}
	keys := make([]string, 0, len(cmpSet))

	for k := range cmpSet {
		keys = append(keys, k)
	}
	sortStrings(keys)

	for _, k := range keys {
		v := cmpSet[k]
		inOld := v&(1<<0) != 0
		inNew := v&(1<<1) != 0

		ptr = ptr.snapshot()
		ptr = ptr.appendKey(k)
		switch {
		case inOld && inNew:
			d.diff(ptr, src[k], tgt[k])
		case inOld && !inNew:
			if _, ok := d.opts.ignores[ptr.string()]; !ok {
				d.remove(ptr.copy(), src[k])
			}
		case !inOld && inNew:
			if _, ok := d.opts.ignores[ptr.string()]; !ok {
				d.add(ptr.copy(), tgt[k])
			}
		}
		ptr = ptr.rewind()
	}
}

// compareArrays generates the patch operations that
// represents the differences between two JSON arrays.
func (d *Differ) compareArrays(ptr pointer, src, tgt []interface{}) {
	ptr = ptr.snapshot()

	size := min(len(src), len(tgt))
	if size < len(src) {
		psize := ptr.appendIndex(size).copy()

		// When the source array contains more elements
		// than the target, entries are being removed
		// from the destination and the removal index
		// is always equal to the original array length.
		for i := size; i < len(src); i++ {
			ptr = ptr.appendIndex(i)

			if _, ok := d.opts.ignores[ptr.string()]; !ok {
				d.remove(psize, src[i])
			}
			ptr = ptr.rewind()
		}
	}
	if d.opts.equivalent && d.unorderedDeepEqualSlice(src, tgt) {
		goto next
	}
	// Compare the elements at each index present in
	// both the source and destination arrays.
	for i := 0; i < size; i++ {
		ptr = ptr.appendIndex(i)
		d.diff(ptr, src[i], tgt[i])
		ptr = ptr.rewind()
	}
next:
	// When the target array contains more elements
	// than the source, entries are appended to the
	// destination.
	for i := size; i < len(tgt); i++ {
		ptr = ptr.appendIndex(i)
		if _, ok := d.opts.ignores[ptr.string()]; !ok {
			ptr = ptr.rewind()
			d.add(ptr.appendKey("-").copy(), tgt[i])
		}
		ptr.rewind()
	}
}

func (d *Differ) unorderedDeepEqualSlice(src, tgt []interface{}) bool {
	if len(src) != len(tgt) {
		return false
	}
	diff := make(map[uint64]int, len(src))

	for _, v := range src {
		k := d.hasher.digest(v)
		diff[k]++
	}
	for _, v := range tgt {
		k := d.hasher.digest(v)
		// If the digest hash is not in the compare,
		// return early.
		if _, ok := diff[k]; !ok {
			return false
		}
		diff[k] -= 1
		if diff[k] == 0 {
			delete(diff, k)
		}
	}
	return len(diff) == 0
}

func (d *Differ) add(ptr string, v interface{}) {
	if !d.opts.factorize {
		d.patch = d.patch.append(OperationAdd, emptyPtr, ptr, nil, v)
		return
	}
	idx := d.findRemoved(v)
	if idx != -1 {
		op := d.patch[idx]

		// https://tools.ietf.org/html/rfc6902#section-4.4f
		// The "from" location MUST NOT be a proper prefix
		// of the "path" location; i.e., a location cannot
		// be moved into one of its children.
		if !strings.HasPrefix(ptr, op.Path) {
			d.patch = d.patch.remove(idx)
			d.patch = d.patch.append(OperationMove, op.Path, ptr, v, v)
		}
		return
	}
	uptr := d.findUnchanged(v)
	if len(uptr) != 0 && !d.opts.invertible {
		d.patch = d.patch.append(OperationCopy, uptr, ptr, nil, v)
	} else {
		d.patch = d.patch.append(OperationAdd, emptyPtr, ptr, nil, v)
	}
}

func (d *Differ) replace(ptr string, src, tgt interface{}) {
	if d.opts.invertible {
		d.patch = d.patch.append(OperationTest, emptyPtr, ptr, nil, src)
	}
	d.patch = d.patch.append(OperationReplace, emptyPtr, ptr, src, tgt)
}

func (d *Differ) remove(ptr string, v interface{}) {
	if d.opts.invertible {
		d.patch = d.patch.append(OperationTest, emptyPtr, ptr, nil, v)
	}
	d.patch = d.patch.append(OperationRemove, emptyPtr, ptr, v, nil)
}

func (d *Differ) findUnchanged(v interface{}) string {
	if d.hashmap != nil {
		k := d.hasher.digest(v)
		node, ok := d.hashmap[k]
		if ok {
			return node.ptr
		}
	}
	return emptyPtr
}

func (d *Differ) findRemoved(v interface{}) int {
	for i := 0; i < len(d.patch); i++ {
		op := d.patch[i]
		if op.Type == OperationRemove && deepEqual(op.OldValue, v) {
			return i
		}
	}
	return -1
}

func (d *Differ) applyOpts(opts ...Option) {
	for _, opt := range opts {
		if opt != nil {
			opt(d)
		}
	}
}

func sortStrings(v []string) {
	if len(v) < 20 {
		insertionSort(v)
	} else {
		sort.Strings(v)
	}
}

func insertionSort(v []string) {
	for j := 1; j < len(v); j++ {
		// Invariant: v[:j] contains the same elements as
		// the original slice v[:j], but in sorted order.
		key := v[j]
		i := j - 1
		for i >= 0 && v[i] > key {
			v[i+1] = v[i]
			i--
		}
		v[i+1] = key
	}
}

func min(i, j int) int {
	if i < j {
		return i
	}
	return j
}
