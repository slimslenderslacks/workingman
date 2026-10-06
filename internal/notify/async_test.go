package notify

import (
	"errors"
	"testing"
	"time"
)

type blockingSender struct{ release chan struct{} }

func (b blockingSender) Send(string, string) error { <-b.release; return errors.New("late failure") }

func TestAsyncDoesNotBlockAndReportsErrors(t *testing.T) {
	release := make(chan struct{})
	errs := make(chan error, 1)
	a := NewAsync(blockingSender{release}, func(err error) { errs <- err })

	returned := make(chan error, 1)
	go func() { returned <- a.Send("t", "m") }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Send = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send blocked on a slow inner sender")
	}

	close(release)
	select {
	case err := <-errs:
		if err == nil || err.Error() != "late failure" {
			t.Fatalf("onError = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inner error was not reported")
	}
}

type asyncPanicSender struct{}

func (asyncPanicSender) Send(string, string) error { panic("boom") }

func TestAsyncRecoversPanics(t *testing.T) {
	errs := make(chan error, 1)
	a := NewAsync(asyncPanicSender{}, func(err error) { errs <- err })
	if err := a.SendTopic("wolf", "t", "m"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("want panic reported as error")
		}
	case <-time.After(time.Second):
		t.Fatal("panic not reported")
	}
}
