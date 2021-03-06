// RUN: llgo -o %t %s
// RUN: %t > %t1 2>&1
// RUN: go run %s > %t2 2>&1
// RUN: diff -u %t1 %t2

package main

func Blah() int {
	println("woobie")
	return 123
}

func F1() (int, float64) {
	return 12, 3.45
}

var X = Y + Blah() // == 579
var Y = 123 + Z    // == 456

var X1, Y1 = F1()

const (
	_ = 333 * iota
	Z
)

var I interface{} = -1
var I1 = I.(int)

func main() {
	println(X, Y)
	println(X1, Y1)
	println(I1)
}
