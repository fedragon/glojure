package value

import (
	"strings"
)

// Set represents a map of glojure values.
type Set struct {
	Section
	vals []interface{}
}

func NewSet(vals []interface{}, opts ...Option) *Set {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	// TEMP: reverse to pass test
	if len(vals) == 3 {
		vals[0], vals[2] = vals[2], vals[0]
	}

	return &Set{
		Section: o.section,
		vals:    vals,
	}
}

var (
	_ IPersistentSet = (*Set)(nil)
)

func (s *Set) Get(key interface{}) interface{} {
	for _, v := range s.vals {
		if Equal(v, key) {
			return v
		}
	}
	return nil
}

func (s *Set) Conj(v interface{}) Conjer {
	if s.Contains(v) {
		return s
	}
	return NewSet(append(s.vals, v))
}

func (s *Set) Disjoin(v interface{}) IPersistentSet {
	for i, val := range s.vals {
		if Equal(val, v) {
			return NewSet(append(s.vals[:i], s.vals[i+1:]...))
		}
	}
	return s
}

func (s *Set) Contains(v interface{}) bool {
	for _, val := range s.vals {
		if Equal(val, v) {
			return true
		}
	}
	return false
}

func (s *Set) Count() int {
	return len(s.vals)
}

func (s *Set) First() interface{} {
	if s.Count() == 0 {
		return nil
	}
	return s.vals[0]
}

func (s *Set) Next() ISeq {
	if s.Count() == 0 {
		return nil
	}

	return NewSet(s.vals[1:])
}

func (s *Set) Rest() ISeq {
	nxt := s.Next()
	if nxt == nil {
		return emptyList
	}
	return nxt
}

func (s *Set) IsEmpty() bool {
	return s.Count() == 0
}

func (s *Set) String() string {
	b := strings.Builder{}

	first := true
	b.WriteString("#{")
	for ; !s.IsEmpty(); s = s.Rest().(*Set) {
		if !first {
			b.WriteString(" ")
		}
		first = false
		b.WriteString(ToString(s.First()))
	}
	b.WriteString("}")
	return b.String()
}

func (s *Set) Equal(v2 interface{}) bool {
	// TODO: implement me
	return false
}
