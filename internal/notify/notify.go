// Package notify 是异步通知派发器。
// 设计要点:不持任何内存队列——每 tick 直接从数据库取 pending 通知,
// 因此服务重启后未送达的通知自然被续发,未闭环事件不会因重启丢失任何环节。
package notify

import (
	"context"
	"log"
	"time"

	"fodsys/internal/store"
)

// Sender 抽象外发通道(短信/甚高频内话/ACARS 等),返回 error 表示外发失败。
type Sender func(ctx context.Context, n channel, subject string, payload any) error

type channel = string

type Worker struct {
	st       *store.Store
	interval time.Duration
	send     Sender
}

func NewWorker(st *store.Store, interval time.Duration, send Sender) *Worker {
	return &Worker{st: st, interval: interval, send: send}
}

// Run 阻塞运行直到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.dispatchOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Worker) dispatchOnce(ctx context.Context) {
	pending, err := w.st.PendingNotifications(ctx, 50)
	if err != nil {
		log.Printf("notify: poll pending: %v", err)
		return
	}
	for _, n := range pending {
		if err := w.send(ctx, n.Channel, n.Subject, n.Payload); err != nil {
			log.Printf("notify: send %s/%s failed: %v (留待下轮重试)", n.Channel, n.ID, err)
			continue // 保持 pending,下一轮重试
		}
		if err := w.st.MarkNotificationSent(ctx, n.ID); err != nil {
			log.Printf("notify: mark sent %s: %v", n.ID, err)
		}
	}
}
