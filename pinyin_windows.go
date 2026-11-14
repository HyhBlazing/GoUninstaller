package main

import (
	"strings"
	"unicode"

	"github.com/mozillazg/go-pinyin"
)

func searchKeys(text string) (full, initials string) {
	var fullB, initB strings.Builder
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			py := pinyin.SinglePinyin(r, args)
			if len(py) == 0 || py[0] == "" {
				continue
			}
			word := strings.ToLower(py[0])
			fullB.WriteString(word)
			initB.WriteByte(word[0])
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			lower := unicode.ToLower(r)
			fullB.WriteRune(lower)
			if unicode.IsLetter(r) {
				initB.WriteRune(lower)
			}
		}
	}
	return fullB.String(), initB.String()
}
