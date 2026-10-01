// Package util holds the small dependency-free helpers the discover packages share.
package util

import (
	"runtime"
	"sync"
)

// ParallelFor runs f(0..n-1) on every CPU.
func ParallelFor(n int, f func(int)) {
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

func Itoa(x int) string {
	if x == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for x > 0 {
		i--
		d[i] = byte('0' + x%10)
		x /= 10
	}
	return string(d[i:])
}
