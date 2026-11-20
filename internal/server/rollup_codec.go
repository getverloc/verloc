package server

// Compact encoding for the per-minute rollup's dimension maps.
//
// These blobs are read on every SeriesSince call, which made them the dominant
// remaining cost of that endpoint (222ms / 64MB to produce 500 points). JSON
// re-parses the same model/backend names once per minute row; the binary form
// writes each name once per blob and needs no reflection.
//
// Measured on a representative minute (60 models + 19 backends):
//
//	JSON   : 180us, 39KB allocated, 357 allocs
//	binary : 4.3us,  ~0 allocated,   0 allocs   (~41x)
//
// Format: a one-byte version tag, then counts and length-prefixed names with
// uvarint values. The tag exists so rows written before this change (raw JSON)
// are still readable — an existing deployment has real history in these columns.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
)

const (
	// dimCodecVersion is written as a leading byte on binary blobs and is not a
	// valid '{' or '[' so it can never be confused with JSON.
	dimCodecVersion = 0x7f
)

var errDimDecode = errors.New("rollup dimension blob is malformed")

// encodeMinutes writes both dimension maps into one buffer. Returns nil for a
// payload that could not be encoded, so the caller can leave the column alone
// rather than writing a half-blob.
func encodeMinutes(dst *bytes.Buffer, models map[string]deltaModel, backends map[string]deltaBackend, hist, ttftHist []uint64) []byte {
	dst.Reset()
	dst.WriteByte(dimCodecVersion)
	// Keys are sorted so the encoding is DETERMINISTIC. Go map iteration order
	// is randomised, so without this identical content encodes to different
	// bytes on every write. That breaks byte-comparison in tests and, more
	// importantly, means a re-written blob never matches the previous one —
	// defeating any future content-addressed caching or change detection.
	putNameCount(dst, len(models))
	for _, k := range sortedKeys(models) {
		v := models[k]
		putName(dst, k)
		putU64(dst, uint64(v.Requests))
		putU64(dst, uint64(v.TokIn))
		putU64(dst, uint64(v.TokOut))
		putU64(dst, uint64(v.TokCache))
		putF64(dst, v.Cost)
	}
	putNameCount(dst, len(backends))
	for _, k := range sortedKeys(backends) {
		v := backends[k]
		putName(dst, k)
		putU64(dst, uint64(v.Requests))
		putU64(dst, uint64(v.TokIn))
		putU64(dst, uint64(v.TokOut))
		putU64(dst, uint64(v.TokCache))
		putU64(dst, uint64(v.Err))
		putF64(dst, v.Cost)
	}
	putU64(dst, uint64(len(hist)))
	for _, v := range hist {
		putU64(dst, v)
	}
	putU64(dst, uint64(len(ttftHist)))
	for _, v := range ttftHist {
		putU64(dst, v)
	}
	out := make([]byte, dst.Len())
	copy(out, dst.Bytes())
	return out
}

// decodeMinutes reads a blob produced by encodeMinutes. It transparently
// accepts legacy JSON: a blob that does not start with the version tag is
// parsed as JSON into the same shape, so history written by an older binary
// stays readable.
func decodeMinutes(blob []byte, models *map[string]deltaModel, backends *map[string]deltaBackend, hist, ttftHist *[]uint64) error {
	if len(blob) == 0 {
		return nil
	}
	if blob[0] != dimCodecVersion {
		return decodeMinutesJSON(blob, models, backends, hist, ttftHist)
	}
	r := bytes.NewReader(blob[1:])
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return errDimDecode
	}
	// Every entry costs at least one byte on the wire, so a count larger
	// than the remaining input is malformed. Check BEFORE preallocating:
	// sizing a map/slice from an unchecked count turns a few corrupt bytes
	// into a gigabyte-scale allocation.
	if n > uint64(r.Len()) {
		return errDimDecode
	}
	if n > 0 {
		m := make(map[string]deltaModel, n)
		for range n {
			name, err := readName(r)
			if err != nil {
				return err
			}
			var v deltaModel
			var reqs, tin, tout, tcache uint64
			var cost float64
			if reqs, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tin, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tout, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tcache, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if cost, err = readF64(r); err != nil {
				return errDimDecode
			}
			v.Requests, v.TokIn, v.TokOut, v.TokCache, v.Cost = int64(reqs), int64(tin), int64(tout), int64(tcache), cost
			m[name] = v
		}
		*models = m
	}
	n, err = binary.ReadUvarint(r)
	if err != nil {
		return errDimDecode
	}
	if n > uint64(r.Len()) {
		return errDimDecode
	}
	if n > 0 {
		b := make(map[string]deltaBackend, n)
		for range n {
			name, err := readName(r)
			if err != nil {
				return err
			}
			var v deltaBackend
			var reqs, tin, tout, tcache, errN uint64
			var cost float64
			if reqs, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tin, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tout, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if tcache, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if errN, err = binary.ReadUvarint(r); err != nil {
				return errDimDecode
			}
			if cost, err = readF64(r); err != nil {
				return errDimDecode
			}
			v.Requests, v.TokIn, v.TokOut, v.TokCache, v.Err, v.Cost = int64(reqs), int64(tin), int64(tout), int64(tcache), int64(errN), cost
			b[name] = v
		}
		*backends = b
	}
	// Histograms are optional in the binary form; a short read means the writer
	// did not include them, which the caller treats as "distributions unknown".
	if n, err = binary.ReadUvarint(r); err == nil && hist != nil {
		h := make([]uint64, 0, capHint(n, r))
		for range n {
			v, err := binary.ReadUvarint(r)
			if err != nil {
				break
			}
			h = append(h, v)
		}
		*hist = h
	}
	if n, err = binary.ReadUvarint(r); err == nil && ttftHist != nil {
		h := make([]uint64, 0, capHint(n, r))
		for range n {
			v, err := binary.ReadUvarint(r)
			if err != nil {
				break
			}
			h = append(h, v)
		}
		*ttftHist = h
	}
	return nil
}

// decodeMinutesInto folds a binary rollup blob directly into destination maps
// and histogram arrays without allocating per-row maps, slices, or key strings
// for already-seen keys. Returns (gotHist, gotTTFT, err). Legacy JSON blobs
// fall back to decodeMinutes transparently.
func decodeMinutesInto(
	blob []byte,
	models map[string]deltaModel,
	backends map[string]deltaBackend,
	hist, ttftHist *[len(latBuckets) + 1]uint64,
) (bool, bool, error) {
	if len(blob) == 0 {
		return false, false, nil
	}
	if blob[0] != dimCodecVersion {
		var mm map[string]deltaModel
		var bb map[string]deltaBackend
		var h, th []uint64
		if err := decodeMinutesJSON(blob, &mm, &bb, &h, &th); err != nil {
			return false, false, err
		}
		for k, v := range mm {
			g := models[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Cost += v.Cost
			models[k] = g
		}
		for k, v := range bb {
			g := backends[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Err += v.Err
			g.Cost += v.Cost
			backends[k] = g
		}
		gotH := len(h) == len(latBuckets)+1
		if gotH {
			for i, v := range h {
				hist[i] += v
			}
		}
		gotTH := len(th) == len(latBuckets)+1
		if gotTH {
			for i, v := range th {
				ttftHist[i] += v
			}
		}
		return gotH, gotTH, nil
	}

	b := blob[1:]
	readU := func() (uint64, bool) {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			return 0, false
		}
		b = b[n:]
		return v, true
	}
	readF := func() (float64, bool) {
		if len(b) < 8 {
			return 0, false
		}
		f := math.Float64frombits(binary.LittleEndian.Uint64(b[:8]))
		b = b[8:]
		return f, true
	}
	readN := func() ([]byte, bool) {
		n, ok := readU()
		if !ok || n > uint64(len(b)) {
			return nil, false
		}
		s := b[:n]
		b = b[n:]
		return s, true
	}

	nModels, ok := readU()
	if !ok || nModels > uint64(len(b)) {
		return false, false, errDimDecode
	}
	for range nModels {
		nameB, ok := readN()
		if !ok {
			return false, false, errDimDecode
		}
		reqs, ok1 := readU()
		tin, ok2 := readU()
		tout, ok3 := readU()
		tcache, ok4 := readU()
		cost, ok5 := readF()
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			return false, false, errDimDecode
		}
		if g, exists := models[string(nameB)]; exists {
			g.Requests += int64(reqs)
			g.TokIn += int64(tin)
			g.TokOut += int64(tout)
			g.TokCache += int64(tcache)
			g.Cost += cost
			models[string(nameB)] = g
		} else {
			models[string(nameB)] = deltaModel{
				Requests: int64(reqs), TokIn: int64(tin), TokOut: int64(tout), TokCache: int64(tcache), Cost: cost,
			}
		}
	}

	nBackends, ok := readU()
	if !ok || nBackends > uint64(len(b)) {
		return false, false, errDimDecode
	}
	for range nBackends {
		nameB, ok := readN()
		if !ok {
			return false, false, errDimDecode
		}
		reqs, ok1 := readU()
		tin, ok2 := readU()
		tout, ok3 := readU()
		tcache, ok4 := readU()
		errN, ok5 := readU()
		cost, ok6 := readF()
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
			return false, false, errDimDecode
		}
		if g, exists := backends[string(nameB)]; exists {
			g.Requests += int64(reqs)
			g.TokIn += int64(tin)
			g.TokOut += int64(tout)
			g.TokCache += int64(tcache)
			g.Err += int64(errN)
			g.Cost += cost
			backends[string(nameB)] = g
		} else {
			backends[string(nameB)] = deltaBackend{
				Requests: int64(reqs), TokIn: int64(tin), TokOut: int64(tout), TokCache: int64(tcache), Err: int64(errN), Cost: cost,
			}
		}
	}

	gotH, gotTH := false, false
	if nh, ok := readU(); ok {
		if nh == uint64(len(latBuckets)+1) {
			var tmp [len(latBuckets) + 1]uint64
			allOK := true
			for i := range tmp {
				v, vok := readU()
				if !vok {
					allOK = false
					break
				}
				tmp[i] = v
			}
			if allOK {
				for i := range tmp {
					hist[i] += tmp[i]
				}
				gotH = true
			}
		} else {
			for range nh {
				if _, vok := readU(); !vok {
					break
				}
			}
		}
	}
	if nth, ok := readU(); ok {
		if nth == uint64(len(latBuckets)+1) {
			var tmp [len(latBuckets) + 1]uint64
			allOK := true
			for i := range tmp {
				v, vok := readU()
				if !vok {
					allOK = false
					break
				}
				tmp[i] = v
			}
			if allOK {
				for i := range tmp {
					ttftHist[i] += tmp[i]
				}
				gotTH = true
			}
		}
	}
	return gotH, gotTH, nil
}

func decodeMinutesJSON(blob []byte, models *map[string]deltaModel, backends *map[string]deltaBackend, hist, ttftHist *[]uint64) error {
	if len(blob) > 0 && blob[0] == '[' {
		// Histogram column.
		if hist != nil {
			return json.Unmarshal(blob, hist)
		}
		if ttftHist != nil {
			return json.Unmarshal(blob, ttftHist)
		}
		return nil
	}
	if models != nil && backends != nil {
		var m map[string]deltaModel
		var b map[string]deltaBackend
		if err := json.Unmarshal(blob, &m); err != nil {
			return err
		}
		if err := json.Unmarshal(blob, &b); err != nil {
			return err
		}
		if m != nil {
			*models = m
		}
		if b != nil {
			*backends = b
		}
		return nil
	}
	if hist != nil {
		return json.Unmarshal(blob, hist)
	}
	if ttftHist != nil {
		return json.Unmarshal(blob, ttftHist)
	}
	return nil
}

// capHint bounds a capacity hint by the bytes actually remaining, so a
// corrupt count cannot force a huge preallocation. Each encoded value
// costs at least one byte, so Len() is a sound upper bound.
func capHint(n uint64, r *bytes.Reader) uint64 {
	if m := uint64(r.Len()); n > m {
		return m
	}
	return n
}

func putNameCount(b *bytes.Buffer, n int) { putU64(b, uint64(n)) }

// sortedKeys returns the map's keys in ascending order. Generic over the value
// type so both dimension maps share one helper.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func putName(b *bytes.Buffer, name string) {
	putU64(b, uint64(len(name)))
	b.WriteString(name)
}

func readName(r *bytes.Reader) (string, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return "", errDimDecode
	}
	if n > uint64(r.Len()) {
		return "", errDimDecode
	}
	buf := make([]byte, n)
	if _, err := r.Read(buf); err != nil {
		return "", errDimDecode
	}
	return string(buf), nil
}

func putU64(b *bytes.Buffer, v uint64) {
	var t [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(t[:], v)
	b.Write(t[:n])
}

func putF64(b *bytes.Buffer, f float64) {
	var t [8]byte
	binary.LittleEndian.PutUint64(t[:], math.Float64bits(f))
	b.Write(t[:])
}

func readF64(r *bytes.Reader) (float64, error) {
	var t [8]byte
	if _, err := r.Read(t[:]); err != nil {
		return 0, errDimDecode
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(t[:])), nil
}
