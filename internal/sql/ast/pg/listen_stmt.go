package pg

type ListenStmt struct {
	Conditionname *string
}

func (n *ListenStmt) Pos() int {
	return 0
}
