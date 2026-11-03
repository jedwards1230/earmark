# runner/testdata

`pilot.py --self-check` transcribes `selfcheck_en.flac` and compares the result
with the text in `selfcheck_en.txt`.

| File | What it is | sha256 |
|---|---|---|
| `selfcheck_en.flac` | 5.86 s, 16 kHz mono 16-bit FLAC, 120 KB | `4e25e22555cd16e90edb0a3b49fdcf1fe652b2a1250ab643634db33895c75b41` |
| `selfcheck_en.txt` | its reference transcript | `c1a46943d4665aac8d39edfa6aed9f345141a9894e352fce1ff540f7725d5506` |

**Source.** Utterance `1272-128104-0000` (speaker 1272, chapter 128104) of the
LibriSpeech ASR corpus, dev-clean subset. It was taken byte-for-byte from the
`hf-internal-testing/librispeech_asr_dummy` copy on Hugging Face.

**License.** LibriSpeech is distributed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/) by V. Panayotov,
G. Chen, D. Povey and S. Khudanpur: "Librispeech: an ASR corpus based on public
domain audio books", ICASSP 2015, <https://www.openslr.org/12>. The underlying
recording is a public-domain LibriVox audiobook. This file is unmodified.
