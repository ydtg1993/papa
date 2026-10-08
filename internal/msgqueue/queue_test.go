package msgqueue

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 容量 <= 0 会夹到 1：channel 容量为 0 是无缓冲，非阻塞发送永远失败 —— 那等于活动/错误全丢。
func TestNewMsgQueueClampsSize(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		q := NewMsgQueue[int](size)
		if cap(q.activities) != 1 || cap(q.errors) != 1 {
			t.Fatalf("size=%d 应夹到 1，实得 activities=%d errors=%d",
				size, cap(q.activities), cap(q.errors))
		}
	}
	q := NewMsgQueue[int](7)
	if cap(q.activities) != 7 || cap(q.errors) != 7 {
		t.Fatalf("合法容量不该被改：activities=%d errors=%d", cap(q.activities), cap(q.errors))
	}
}

// SendError 绝不能阻塞调用方：它跑在 worker / 刷新协程里，阻塞就是拖着业务跑。
func TestSendErrorNeverBlocks(t *testing.T) {
	q := NewMsgQueue[int](1)
	q.SendError(errors.New("first"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			q.SendError(errors.New("overflow"))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SendError 在通道满时阻塞了")
	}
	if got := len(q.Errors()); got != 1 {
		t.Fatalf("满即丢，通道里应只剩 1 条，实得 %d", got)
	}
}

// 活动丢失本身不算故障，但必须留痕 —— 否则监控上"少了几个 worker 的活动"无从解释。
func TestSendActivityDropIsRecorded(t *testing.T) {
	q := NewMsgQueue[string](1)
	q.SendActivity("占位") // 占满活动通道
	q.SendActivity("该丢")

	select {
	case err := <-q.Errors():
		if !strings.Contains(err.Error(), "dropped") {
			t.Fatalf("留痕内容 = %q，应说明活动被丢", err.Error())
		}
	default:
		t.Fatal("活动被丢弃时应在错误通道留一条痕")
	}

	// 错误通道也满时，再丢活动不能阻塞、也不能 panic
	q.SendError(errors.New("占满错误通道"))
	q.SendActivity("继续丢")
}

// Activities / Errors 返回的就是内部通道本身（消费方只有一个），不该是副本。
func TestAccessorsReturnLiveChannels(t *testing.T) {
	q := NewMsgQueue[int](2)
	q.SendActivity(7)
	select {
	case got := <-q.Activities():
		if got != 7 {
			t.Fatalf("Activities() = %d, want 7", got)
		}
	default:
		t.Fatal("SendActivity 之后通道里应当有值")
	}

	want := errors.New("boom")
	q.SendError(want)
	select {
	case got := <-q.Errors():
		if !errors.Is(got, want) {
			t.Fatalf("Errors() 拿到的是另一个 error：%v", got)
		}
	default:
		t.Fatal("SendError 之后通道里应当有值")
	}
}
