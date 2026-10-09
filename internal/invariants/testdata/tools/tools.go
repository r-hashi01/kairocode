package tools

type Effect int

type Def struct {
	Name   string
	Effect Effect
}

var Declared = Def{Name: "read", Effect: 2}

var Undeclared = Def{Name: "write"}
