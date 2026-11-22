//go:build race

package chat_test

// slack loosens timing targets under the race detector, which slows everything several times.
const slack = 4
