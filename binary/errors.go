package binary

import "errors"

var (
	ErrInvalidType    = errors.New("unsupported payload type")
	ErrStreamEnd      = errors.New("stream end")
	ErrInvalidJIDType = errors.New("invalid JID type")
	ErrInvalidNode    = errors.New("invalid node")
	ErrInvalidToken   = errors.New("invalid token with tag")
	ErrNonStringKey   = errors.New("non-string key")
)