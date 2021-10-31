package lrmap

import (
	"math"
	"testing"
)

func TestHundred(t *testing.T) {
	lrm := New[int, int]()

	rh := lrm.NewReadHandler()
	defer rh.Close()

	for i := 1; i < 100; i += 2 {
		k, v := i, i

		lrm.Set(k, v)
	}

	lrm.Flush()

	for i := 1; i < 100; i += 2 {
		k, v := i, i

		rh.Enter()
		if _v := rh.Get(k); _v != v {

			t.Errorf("Get(%d) want %d, got %d", k, v, _v)
		}
		rh.Leave()
	}

	for i := 0; i < 100; i += 2 {
		k, v := i, i
		lrm.Set(k, v)
	}

	lrm.Flush()

	for i := 0; i < 100; i++ {
		rh.Enter()
		k, v := i, i

		if _v := rh.Get(k); _v != v {
			t.Errorf("Get(%d) want %d, got %d", k, v, _v)
		}
		rh.Leave()
	}

	lrm.Flush()

	rh.Enter()
	for i := 0; i < 100; i++ {
		k, v := i, i

		if _v := rh.Get(k); _v != v {
			t.Errorf("Get(%d) want %d, got %d", k, v, _v)
		}
	}
	rh.Leave()

	for i := 0; i < 100; i += 2 {
		k := i
		lrm.Delete(k)
	}

	lrm.Flush()

	rh.Enter()
	for i := 0; i < 100; i++ {
		k, v := i, i

		_v, ok := rh.GetOK(k)
		if i%2 == 0 {
			if ok || _v != 0 {
				t.Errorf("GetOK(%d), want (0, false), got (%d, %t)", i, _v, ok)
			}
		} else {
			if !ok || _v != v {
				t.Errorf("GetOK(%d), want (%d, true), got (%d, %t)", i, v, _v, ok)
			}
		}
	}
	rh.Leave()

	lrm.Flush()

	rh.Enter()
	for i := 0; i < 100; i++ {
		k, v := i, i

		_v, ok := rh.GetOK(k)
		if i%2 == 0 {
			if ok || _v != 0 {
				t.Errorf("GetOK(%d), want (0, false), got (%d, %t)", i, _v, ok)
			}
		} else {
			if !ok || _v != v {
				t.Errorf("GetOK(%d), want (%d, true), got (%d, %t)", i, v, _v, ok)
			}
		}
	}
	rh.Leave()
}

func TestEpochOverflow(t *testing.T) {
	var maxEven uint64 = math.MaxUint64 - 1
	t.Logf("maxEven(%d)", maxEven)
	if maxEven%2 == 1 {
		t.Errorf("maxEven(%d) is odd", maxEven)
	}

	maxOdd := maxEven + 1
	t.Logf("maxOdd(%d)", maxOdd)
	if maxOdd%2 == 0 {
		t.Errorf("maxOdd(%d) is even", maxOdd)
	}

	overflow := maxOdd + 1
	t.Logf("overflow(%d)", overflow)
	if overflow%2 == 1 {
		t.Errorf("overflow(%d) is odd", overflow)
	}
}
