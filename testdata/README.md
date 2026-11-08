# Test fixtures

`legacy-methods.zip` holds three members copied byte-for-byte (compressed data and headers) from
art packs published on 16colo.rs, to exercise the PKZIP 1.x decompressors:

| Member | Method | Source |
| --- | --- | --- |
| `implode-4k-2trees.ans` | Implode, 4K dictionary, two trees | `WAVEHAND.ANS` from the 1991 pack `ensiart` |
| `implode-8k-3trees.ans` | Implode, 8K dictionary, three trees | `RAIDERS.ANS` from the 1991 pack `ensiart` |
| `shrink.ans` | Shrink | `TEST.ANS` from the 1990 pack `eansis` |

Info-ZIP `unzip` extracts all three with matching CRCs, which is the reference the decoders were
verified against.
