package main

import (
	"fmt"
	"jenkins_test/hello"
)

func main() {
	str := hello.ReturnHello("Jenkins!")
	fmt.Println(str)
}
