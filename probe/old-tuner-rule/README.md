# Why the A C A C A tuner failed in hash order

Monte Carlo models of the old tuner's round evaluation (`internal/tuner/tuner.go`
at `31beeef`, `evaluate` and `plausible`), written on 2026-10-13 to explain why
it measured 4 -> 8 readers as -10% in hash order when pinned runs showed +79%.
Each arm's log rate is the true level of its setting plus independent noise of
spread sigma, standing in for which slow and fast files happened to be open.
Standard library only; each runs in seconds:

```
python3 -I probe/old-tuner-rule/rule_mc.py
python3 -I probe/old-tuner-rule/clip_bias.py
python3 -I probe/old-tuner-rule/clamp_vs_void.py
```

- `rule_mc.py`: the whole verdict (region-change check, plausibility check,
  keep rule) for 4 -> 8 readers (true +79%) and 4 -> 3 (true -21%). At sigma
  0.5, 12% of rounds keep the right answer, 70% are thrown out, and the rounds
  that count average +28%.
- `clip_bias.py`: the plausibility check alone. It voids readings outside
  1/2.3x to 2.3x for a doubling, a range centered on "no change", so the
  bigger the true gain, the more of its high readings are thrown out and the
  lower the survivors average (1.79x true reads as 1.51x at sigma 0.3).
- `clamp_vs_void.py`: capping out-of-range readings instead of voiding them
  roughly halves that bias but doesn't remove it.

The tuner they model was replaced at v3: it now measures only reads that
never got stuck, which removes the file mix from the measurement. See
`docs/built/qclone-v3-built.html`.
