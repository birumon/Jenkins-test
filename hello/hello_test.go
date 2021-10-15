package hello

import (
	"testing"
)

func TestReturnHello(t *testing.T) {
	msg := ReturnHello("Jenkins!")
	if msg != "Hello, Jenkins!" {
		t.Fatalf(`ReturnHello("Jenkins!") = %q, want "Hello, Jenkins!"`, msg)
	}
}

func TestReturnHelloEmpty(t *testing.T) {
	msg := ReturnHello("")
	if msg != "Hello, " {
		t.Fatalf(`ReturnHello("") = %q, want "Hello, "`, msg)
	}
}
