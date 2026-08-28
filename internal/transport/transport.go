package transport

import (
	"context"
	"github.com/ximengyi/taskian/internal/message"
)

type Transport interface {
	Name() string
	Poll(context.Context) ([]message.Incoming, error)
	Send(context.Context, string, string) error
	Close() error
}
