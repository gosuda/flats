package model

import "math"

// EndpointWorld is the canvas coordinate of an end attached at the
// normalized point at of an element with geometry g: the point inside the
// unrotated box, rotated by g.Rot degrees (clockwise on screen) around the
// box center, rounded like every stored number. The JS client implements the
// same function; testdata/geometry.json keeps them equal.
func EndpointWorld(g Geom, at [2]float64) [2]float64 {
	cx, cy := g.X+g.W/2, g.Y+g.H/2
	dx, dy := g.X+at[0]*g.W-cx, g.Y+at[1]*g.H-cy
	sin, cos := sinCos(g.Rot)
	return [2]float64{Round1(cx + dx*cos - dy*sin), Round1(cy + dx*sin + dy*cos)}
}

// sinCos is exact at multiples of 90 degrees, where math.Sincos leaves
// rounding residue that a client could round differently.
func sinCos(deg float64) (float64, float64) {
	switch math.Mod(deg, 360) {
	case 0:
		return 0, 1
	case 90:
		return 1, 0
	case 180:
		return 0, -1
	case 270:
		return -1, 0
	}
	return math.Sincos(deg * math.Pi / 180)
}

// ConnectorBounds is a connector's derived box: the bounding box of its two
// end coordinates, unrotated. A straight or axis-aligned connector has a zero
// width or height.
func ConnectorBounds(p0, p1 [2]float64) Geom {
	return Geom{
		X: math.Min(p0[0], p1[0]),
		Y: math.Min(p0[1], p1[1]),
		W: math.Abs(p1[0] - p0[0]),
		H: math.Abs(p1[1] - p0[1]),
	}
}
