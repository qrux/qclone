# Monte Carlo of qclone's tuner.evaluate() for the readers dimension (tuner.go:640-700).
# Each arm's log rate = true level of its setting + iid N(0, sigma) "which files were open".
import math, random
L = math.log
upGain, downLoss, downWorst = L(1.05), L(1.02), L(1.06)
driftLimit, slack = L(2.5), L(1.15)
def round_(eff, ratio, up, sig, rng):
    a1, a2, a3 = (rng.gauss(0, sig) for _ in range(3))
    c1, c2 = (eff + rng.gauss(0, sig) for _ in range(2))
    if max(abs(a1-a2), abs(a2-a3)) > driftLimit: return "region changed", None
    s1, s2 = c1-(a1+a2)/2, c2-(a2+a3)/2
    bound = abs(L(ratio)) + slack
    if abs(s1) > bound or abs(s2) > bound: return "implausible", None
    mean, worst, noise = (s1+s2)/2, min(s1, s2), abs(a1-2*a2+a3)/2
    if up: keep = mean > max(upGain, noise) and worst > 0
    else:  keep = (mean > -downLoss and worst > -downWorst) or (mean > max(upGain, noise) and worst > 0)
    return ("kept" if keep else "rejected"), mean
rng = random.Random(1); N = 200000
# 4 -> 8 readers: pinned hash/4-files says +79%.  4 -> 3: rate ~ readers^0.84 gives -21%.
cases = [("4->8 (true +79%)", L(1.79), 2.0, True), ("4->3 (true -21%)", 0.84*L(0.75), 0.75, False)]
print("sigma = spread of one arm's log rate from the file mix (0.5 ~ arm off by x1.65 either way)")
for name, eff, ratio, up in cases:
    print("\n" + name)
    print(" sigma  kept  rejected  region-chg  implausible  | mean gain of rounds that counted")
    for sig in (0.05, 0.15, 0.3, 0.5, 0.7):
        n = {}; g = []
        for _ in range(N):
            v, m = round_(eff, ratio, up, sig, rng); n[v] = n.get(v, 0) + 1
            if m is not None: g.append(m)
        f = lambda k: 100*n.get(k, 0)/N
        mg = (math.exp(sum(g)/len(g))-1)*100 if g else float('nan')
        print(f" {sig:4.2f}  {f('kept'):4.0f}%  {f('rejected'):6.0f}%  {f('region changed'):8.0f}%  {f('implausible'):9.0f}%    | {mg:+5.0f}%")
