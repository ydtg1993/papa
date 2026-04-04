package pkg

import "fmt"

type MsgQueue[T any] struct {
	activities chan T
	errors     chan error
}

func NewMsgQueue[T any](size int) *MsgQueue[T] {
	if size <= 0 {
		size = 1
	}
	return &MsgQueue[T]{
		activities: make(chan T, size),
		errors:     make(chan error, size),
	}
}

// SendActivity 非阻塞发送活动信息
func (m *MsgQueue[T]) SendActivity(activity T) {
	select {
	case m.activities <- activity:
	default:
		select {
		case m.errors <- fmt.Errorf("activity send to channel dropped"):
		default:
			break
		}
	}
}

// SendError 非阻塞发送错误
func (m *MsgQueue[T]) SendError(err error) {
	select {
	case m.errors <- err:
	default:
		break
	}
}

// Errors 返回错误通道
func (m *MsgQueue[T]) Errors() <-chan error {
	return m.errors
}

func (m *MsgQueue[T]) Activities() <-chan T {
	return m.activities
}
