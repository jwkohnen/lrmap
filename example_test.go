package lrmap

import (
	"fmt"
	"sort"
)

func ExampleReadHandler_Iterate() {
	m := New[int, int]()

	for i := 0; i < 10; i++ {
		k, v := i, i
		m.Set(k, v)
	}

	m.Commit()

	rh := m.NewReadHandler()
	rh.Enter()
	defer rh.Close()

	var values []int

	rh.Iterate(func(_ int, v int) bool {
		values = append(values, v)

		return true
	})

	sort.Ints(values)
	fmt.Println(values)

	// Output:
	// [0 1 2 3 4 5 6 7 8 9]
}
