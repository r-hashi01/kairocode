package bad

import (
	"maps"
	"math/rand"
	"os"
	"time"
)

func Turn(m map[string]int) int {
	t := time.Now()    // clock
	go func() {}()     // goroutine
	select {}          // select
	for k := range m { // map order
		_ = k
	}
	for k := range maps.Keys(m) { // map order through an iterator
		_ = k
	}
	_ = os.Getenv("HOME")
	return rand.Int() + t.Second()
}
