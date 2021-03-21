package lrmap_test

import (
	"fmt"
	"sort"

	"github.com/jwkohnen/lrmap"
)

func ExampleReadHandler_Iterate() {
	m := lrmap.New()

	for i := 0; i < 10; i++ {
		k, v := lrmap.Key(i), lrmap.Value(i)
		m.Set(k, v)
	}

	m.Flush()

	rh := m.NewReadHandler()
	rh.Enter()
	defer rh.Close()

	var values []int

	rh.Iterate(func(_ lrmap.Key, v lrmap.Value) bool {
		values = append(values, int(v))

		return true
	})

	sort.Ints(values)
	fmt.Println(values)

	// Output:
	// [0 1 2 3 4 5 6 7 8 9]
}
