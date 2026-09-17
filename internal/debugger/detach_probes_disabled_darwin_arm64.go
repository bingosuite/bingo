//go:build darwin && arm64 && bingonative && !e2e

package debugger

import "context"

type darwinWaitHooks struct{}

func (*darwinWaitHooks) at(string) error                               { return nil }
func (*darwinWaitHooks) afterReceive(int) error                        { return nil }
func (*darwinWaitHooks) atContext(ctx context.Context, _ string) error { return ctx.Err() }
func (*darwinWaitHooks) received(bool)                                 {}
func (*darwinWaitHooks) rendezvousArmed(int)                           {}
func (*darwinWaitHooks) rendezvoused(int, uint32)                      {}
func (*darwinWaitHooks) patched() error                                { return nil }
