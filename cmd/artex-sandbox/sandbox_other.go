//go:build !linux

package main

import "errors"

// uid 바꾸기와 cap 버리기는 Linux에서만 한다. 다른 시스템에서 켜면 서버의 자기 검사가 이 오류로 멈춘다.
func run([]string) error {
	return errors.New("agent uid separation is supported only on Linux")
}
