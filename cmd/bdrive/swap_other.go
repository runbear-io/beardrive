//go:build !darwin && !linux

package main

func swapFiles(a, b string) error { return errNoSwap }
