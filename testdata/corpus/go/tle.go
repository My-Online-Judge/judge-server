package main

import "fmt"

// Case 1 never ends (CPU time limit); the others answer.
func main() {
	var a, b int64
	fmt.Scan(&a, &b)
	if a == 1 && b == 2 {
		var spin uint64
		for {
			spin++
		}
	}
	fmt.Println(a + b)
}
