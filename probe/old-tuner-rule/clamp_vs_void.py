# Same rounds, two rules: void out-of-window rounds (today) vs clamp each half to the window.
import math, random
L = math.log; b = L(2) + L(1.15); rng = random.Random(3); N = 200000
cl = lambda s: max(-b, min(b, s))
print("true gain  sigma   void: avg (counted)   clamp: avg (all count)")
for g in (1.0, 1.4, 1.79, 2.0):
    for sig in (0.3, 0.5):
        v = []; c = []
        for _ in range(N):
            a1, a2, a3 = (rng.gauss(0, sig) for _ in range(3))
            c1, c2 = (L(g) + rng.gauss(0, sig) for _ in range(2))
            s1, s2 = c1-(a1+a2)/2, c2-(a2+a3)/2
            if abs(s1) <= b and abs(s2) <= b: v.append((s1+s2)/2)
            c.append((cl(s1)+cl(s2))/2)
        m = lambda xs: math.exp(sum(xs)/len(xs))
        print(f"  x{g:<5}   {sig:.1f}     x{m(v):.2f} ({100*len(v)/N:3.0f}%)        x{m(c):.2f}")
