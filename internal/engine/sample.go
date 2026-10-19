package engine

import "math"

// betaDraw samples Beta(a, b) from two gamma draws, X / (X + Y), on the
// uniform source rnd, a float in [0, 1). The shapes must be positive.
func betaDraw(rnd func() float64, a, b float64) float64 {
	x, y := gammaDraw(rnd, a), gammaDraw(rnd, b)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// gammaDraw samples Gamma(shape, 1) by Marsaglia and Tsang's method. A
// shape below 1 is raised by one and scaled back by U^(1/shape), the
// Ahrens-Dieter boost.
func gammaDraw(rnd func() float64, shape float64) float64 {
	if shape < 1 {
		return gammaDraw(rnd, shape+1) * math.Pow(openUnit(rnd), 1/shape)
	}
	d := shape - 1.0/3
	c := 1 / math.Sqrt(9*d)
	// The loop accepts with a probability above 0.95 per round; the bound
	// only keeps a degenerate source (a constant) from spinning forever.
	for range 1000 {
		x := normalDraw(rnd)
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		if math.Log(openUnit(rnd)) < 0.5*x*x+d-d*v+d*math.Log(v) {
			return d * v
		}
	}
	return d
}

// normalDraw is a standard normal by the Box-Muller transform.
func normalDraw(rnd func() float64) float64 {
	return math.Sqrt(-2*math.Log(openUnit(rnd))) * math.Cos(2*math.Pi*rnd())
}

// openUnit draws from (0, 1], so a logarithm of it is finite.
func openUnit(rnd func() float64) float64 {
	return 1 - rnd()
}
