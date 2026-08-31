package lexorg

//go:generate stringer -type=errGeneric -linecomment -output=stringers.go .
type errGeneric uint8

const (
	_                errGeneric = iota // lexorg: undefined error
	ErrUninitialized                   // lexorg: need initialization
	ErrRewound                         // lexorg: offset precedes retained stream bytes
	ErrViewTooLarge                    // lexorg: view longer than fill buffer
)

func (eg errGeneric) Error() string {
	return eg.String()
}
