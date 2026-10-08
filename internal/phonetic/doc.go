// Package phonetic answers "do these two phrases sound alike?" for the decide
// step's rung-0 checks (internal/decide).
//
// It holds a Double Metaphone encoder ported from Apache Commons Codec (see
// doublemetaphone.go for attribution; the port is checked against
// commons-codec's own golden vectors), a small number-to-words speller so
// "240" and "two hundred forty" compare as the same sound, and SoundAlike, a
// phrase-level similarity built from both.
//
// Everything here is pure: no I/O, no clock, no randomness, no state.
package phonetic
