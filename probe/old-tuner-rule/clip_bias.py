# The plausibility check alone (tuner.go:786): how far below the truth do surviving rounds average?
import math, random
L = math.log; bound = L(2) + L(1.15); rng = random.Random(2); N = 200000
print("window: C/A from %.3f to %.2f" % (math.exp(-bound), math.exp(bound)))
print("true gain  sigma  voided  avg of survivors")
for g in (1.0, 1.4, 1.79, 2.0):
    for sig in (0.3, 0.5):
        kept = []; void = 0
        for _ in range(N):
            a1, a2, a3 = (rng.gauss(0, sig) for _ in range(3))
            c1, c2 = (L(g) + rng.gauss(0, sig) for _ in range(2))
            s1, s2 = c1-(a1+a2)/2, c2-(a2+a3)/2
            if abs(s1) > bound or abs(s2) > bound: void += 1
            else: kept.append((s1+s2)/2)
        print(f"  x{g:<5}   {sig:.1f}   {100*void/N:4.0f}%   x{math.exp(sum(kept)/len(kept)):.2f}")
