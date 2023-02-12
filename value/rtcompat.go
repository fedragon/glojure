package value

import "sync/atomic"

var (
	RT = &RTMethods{}
)

// RT is a struct with methods that map to Clojure's RT class' static
// methods. This approach is used to make translation of core.clj to
// Glojure easier.
type RTMethods struct {
	id atomic.Int32
}

func (rt *RTMethods) NextID() int {
	return int(rt.id.Add(1))
}

func (rt *RTMethods) Nth(x interface{}, i int) interface{} {
	return MustNth(x, i)
}

func (rt *RTMethods) NthDefault(x interface{}, i int, def interface{}) interface{} {
	v, ok := Nth(x, i)
	if !ok {
		return def
	}
	return v
}

func (rt *RTMethods) Dissoc(x interface{}, k interface{}) interface{} {
	return Dissoc(x, k)
}
