package point

// MultiExpCore ...
const MultiExpCore = `

// MultiExp implements section 4 of https://eprint.iacr.org/2012/549.pdf 
// optionally, takes as parameter a CPUSemaphore struct
// enabling to set max number of cpus to use
func (p *{{ toUpper .PointName }}Jac) MultiExp(points []{{ toUpper .PointName }}Affine, scalars []fr.Element, opts ...*CPUSemaphore) *{{ toUpper .PointName }}Jac {
	// note: 
	// each of the msmCX method is the same, except for the c constant it declares
	// duplicating (through template generation) these methods allows to declare the buckets on the stack
	// the choice of c needs to be improved: 
	// there is a theoritical value that gives optimal asymptotics
	// but in practice, other factors come into play, including:
	// * if c doesn't divide 64, the word size, then we're bound to select bits over 2 words of our scalars, instead of 1
	// * number of CPUs 
	// * cache friendliness (which depends on the host, G1 or G2... )
	//	--> for example, on BN256, a G1 point fits into one cache line of 64bytes, but a G2 point don't. 

	// for each msmCX
	// step 1
	// we compute, for each scalars over c-bit wide windows, nbChunk digits
	// if the digit is larger than 2^{c-1}, then, we borrow 2^c from the next window and substract
	// 2^{c} to the current digit, making it negative.
	// negative digits will be processed in the next step as adding -G into the bucket instead of G
	// (computing -G is cheap, and this saves us half of the buckets)
	// step 2
	// buckets are declared on the stack
	// notice that we have 2^{c-1} buckets instead of 2^{c} (see step1)
	// we use jacobian extended formulas here as they are faster than mixed addition
	// msmProcessChunk places points into buckets base on their selector and return the weighted bucket sum in given channel
	// step 3
	// reduce the buckets weigthed sums into our result (msmReduceChunk)

	var opt *CPUSemaphore
	if len(opts) > 0 {
		opt = opts[0]
	} else {
		opt = NewCPUSemaphore(runtime.NumCPU())
	}

	var C uint64
	nbPoints := len(points)

	// implemented msmC methods (the c we use must be in this slice)
	implementedCs := []uint64{
		{{- range $c :=  .CRange}} {{- if and (eq $.PointName "g1") (gt $c 21)}}{{- else}} {{$c}},{{- end}}{{- end}}
	}

	// approximate cost (in group operations)
	// cost = bits/c * (nbPoints + 2^{c})
	// this needs to be verified empirically. 
	// for example, on a MBP 2016, for G2 MultiExp > 8M points, hand picking c gives better results
	min := math.MaxFloat64
	for _, c := range implementedCs {
		cc := fr.Limbs * 64 * (nbPoints + (1 << (c)))
		cost := float64(cc) / float64(c)
		if cost < min {
			min = cost
			C = c 
		}
	}

	// empirical, needs to be tuned.
	{{if eq .PointName "g1"}}
	// if C > 16 && nbPoints < 1 << 23 {
	// 	C = 16
	// } 
	{{else}}
	// if C > 16 && nbPoints < 1 << 23 {
	// 	C = 16
	// }
	{{end}}
	


	// take all the cpus to ourselves
	opt.lock.Lock()

	// partition the scalars 
	// note: we do that before the actual chunk processing, as for each c-bit window (starting from LSW)
	// if it's larger than 2^{c-1}, we have a carry we need to propagate up to the higher window
	scalars = partitionScalars(scalars, C)

	switch C {
	{{range $c :=  .CRange}}
	case {{$c}}:
		return p.msmC{{$c}}(points, scalars, opt)	
	{{end}}
	default:
		panic("unimplemented")
	}
}

// msmReduceChunk{{ toUpper .PointName }} reduces the weighted sum of the buckets into the result of the multiExp
func msmReduceChunk{{ toUpper .PointName }}(p *{{ toUpper .PointName }}Jac, c int, chChunks []chan {{ toUpper .PointName }}Jac)  *{{ toUpper .PointName }}Jac {
	totalj := <-chChunks[len(chChunks)-1]
	p.Set(&totalj)
	for j := len(chChunks) - 2; j >= 0; j-- {
		for l := 0; l < c; l++ {
			p.DoubleAssign()
		}
		totalj := <-chChunks[j]
		p.AddAssign(&totalj)
	}
	return p
}


func msmProcessChunk{{ toUpper .PointName }}(chunk uint64,
	 chRes chan<- {{ toUpper .PointName }}Jac,
	 buckets []{{ toLower .PointName }}JacExtended,
	 c uint64,
	 points []{{ toUpper .PointName }}Affine,
	 scalars []fr.Element) {


	mask  := uint64((1 << c) - 1)	// low c bits are 1
	msbWindow  := uint64(1 << (c -1)) 
	
	for i := 0 ; i < len(buckets); i++ {
		buckets[i].setInfinity()
	}

	jc := uint64(chunk * c)
	s := selector{}
	s.index = jc / 64
	s.shift = jc - (s.index * 64)
	s.mask = mask << s.shift
	s.multiWordSelect = (64 %c)!=0   && s.shift > (64-c) && s.index < (fr.Limbs - 1 )
	if s.multiWordSelect {
		nbBitsHigh := s.shift - uint64(64-c)
		s.maskHigh = (1 << nbBitsHigh) - 1
		s.shiftHigh = (c - nbBitsHigh)
	}


	// for each scalars, get the digit corresponding to the chunk we're processing. 
	for i := 0; i < len(scalars); i++ {
		bits := (scalars[i][s.index] & s.mask) >> s.shift
		if s.multiWordSelect {
			bits += (scalars[i][s.index+1] & s.maskHigh) << s.shiftHigh
		}

		if bits == 0 {
			continue
		}
		
		// if msbWindow bit is set, we need to substract
		if bits & msbWindow == 0 {
			// add 
			buckets[bits-1].add(&points[i])
		} else {
			// sub
			buckets[bits & ^msbWindow].sub(&points[i])
		}
	}

	
	// reduce buckets into total
	// total =  bucket[0] + 2*bucket[1] + 3*bucket[2] ... + n*bucket[n-1]

	var runningSum, tj, total {{ toUpper .PointName }}Jac
	runningSum.Set(&{{ toLower .PointName }}Infinity)
	total.Set(&{{ toLower .PointName }}Infinity)
	for k := len(buckets) - 1; k >= 0; k-- {
		if !buckets[k].ZZ.IsZero() {
			runningSum.AddAssign(tj.unsafeFromJacExtended(&buckets[k]))
		}
		total.AddAssign(&runningSum)
	}
	

	chRes <- total
	close(chRes)
} 


{{range $c :=  .CRange}}

func (p *{{ toUpper $.PointName }}Jac) msmC{{$c}}(points []{{ toUpper $.PointName }}Affine, scalars []fr.Element, opt *CPUSemaphore) *{{ toUpper $.PointName }}Jac {
	{{- $cDividesBits := divides $c $.RBitLen}}
	const c  = {{$c}} 							// scalars partitioned into c-bit radixes
	const nbChunks = (fr.Limbs * 64 / c) {{if not $cDividesBits }} + 1 {{end}} // number of c-bit radixes in a scalar
	
	// for each chunk, spawn a go routine that'll loop through all the scalars
	var chChunks [nbChunks]chan {{ toUpper $.PointName }}Jac

	// wait group to wait for all the go routines to start
	var wg sync.WaitGroup
	
	{{- if not $cDividesBits }}
	// c doesn't divide {{$.RBitLen}}, last window is smaller we can allocate less buckets
	const lastC = (fr.Limbs * 64) - (c * (fr.Limbs * 64 / c))
	chChunks[nbChunks-1] = make(chan {{ toUpper $.PointName }}Jac, 1)
	<-opt.chCpus  // wait to have a cpu before scheduling 
	wg.Add(1)
	go func(j uint64, chRes chan {{ toUpper $.PointName }}Jac, points []{{ toUpper $.PointName }}Affine, scalars []fr.Element) {
		wg.Done()
		var buckets [1<<(lastC-1)]{{ toLower $.PointName }}JacExtended
		msmProcessChunk{{ toUpper $.PointName }}(j, chRes, buckets[:], c, points, scalars)
		opt.chCpus <- struct{}{} // release token in the semaphore
	}(uint64(nbChunks-1), chChunks[nbChunks-1], points, scalars)

	for chunk := nbChunks - 2; chunk >= 0; chunk-- {
	{{ else}}
	for chunk := nbChunks - 1; chunk >= 0; chunk-- {
	{{- end}}
		chChunks[chunk] = make(chan {{ toUpper $.PointName }}Jac, 1)
		<-opt.chCpus  // wait to have a cpu before scheduling 
		wg.Add(1)
		go func(j uint64, chRes chan {{ toUpper $.PointName }}Jac, points []{{ toUpper $.PointName }}Affine, scalars []fr.Element) {
			wg.Done()
			var buckets [1<<(c-1)]{{ toLower $.PointName }}JacExtended
			msmProcessChunk{{ toUpper $.PointName }}(j, chRes,  buckets[:], c, points, scalars)
			opt.chCpus <- struct{}{} // release token in the semaphore
		}(uint64(chunk), chChunks[chunk], points, scalars)
	}

	// wait for all goRoutines to actually start
	wg.Wait()

	// all my tasks are scheduled, I can let other func use avaiable tokens in the semaphore
	opt.lock.Unlock() 
	return msmReduceChunk{{ toUpper $.PointName }}(p, c, chChunks[:])
}
{{end}}


//  {{ toLower .PointName }}JacExtended parameterized jacobian coordinates (x=X/ZZ, y=Y/ZZZ, ZZ**3=ZZZ**2)
type {{ toLower .PointName }}JacExtended struct {
	X, Y, ZZ, ZZZ {{.CoordType}}
}

// setInfinity sets p to O
func (p *{{ toLower .PointName }}JacExtended) setInfinity() *{{ toLower .PointName }}JacExtended {
	p.X.SetOne()
	p.Y.SetOne()
	p.ZZ = {{.CoordType}}{}
	p.ZZZ = {{.CoordType}}{}
	return p
}

// fromJacExtended sets Q in affine coords
func (p *{{ toUpper .PointName }}Affine)  fromJacExtended (Q *{{ toLower .PointName }}JacExtended) *{{ toUpper .PointName }}Affine {
	if Q.ZZ.IsZero() {
		p.X = {{.CoordType}}{}
		p.Y = {{.CoordType}}{}
		return p
	}
	p.X.Inverse(&Q.ZZ).Mul(&p.X, &Q.X)
	p.Y.Inverse(&Q.ZZZ).Mul(&p.Y, &Q.Y)
	return p
}

// fromJacExtended sets Q in Jacobian coords
func (p *{{ toUpper .PointName }}Jac) fromJacExtended(Q *{{ toLower .PointName }}JacExtended) *{{ toUpper .PointName }}Jac {
	if Q.ZZ.IsZero() {
		p.Set(&{{ toLower .PointName }}Infinity)
		return p
	}
	p.X.Mul(&Q.ZZ, &Q.X).Mul(&p.X, &Q.ZZ)
	p.Y.Mul(&Q.ZZZ, &Q.Y).Mul(&p.Y, &Q.ZZZ)
	p.Z.Set(&Q.ZZZ)
	return p
}

// unsafeFromJacExtended sets p in jacobian coords, but don't check for infinity
func (p *{{ toUpper .PointName }}Jac) unsafeFromJacExtended(Q *{{ toLower .PointName }}JacExtended) *{{ toUpper .PointName }}Jac {
	p.X.Square(&Q.ZZ).Mul(&p.X, &Q.X)
	p.Y.Square(&Q.ZZZ).Mul(&p.Y, &Q.Y)
	p.Z = Q.ZZZ
	return p
}


// sub same as add, but will negate a.Y 
// http://www.hyperelliptic.org/EFD/ {{ toLower .PointName }}p/auto-shortw-xyzz.html#addition-madd-2008-s
func (p *{{ toLower .PointName }}JacExtended) sub(a *{{ toUpper .PointName }}Affine) *{{ toLower .PointName }}JacExtended {
	{{ template "add" dict "all" . "negate" true}}
}


// add
// http://www.hyperelliptic.org/EFD/ {{ toLower .PointName }}p/auto-shortw-xyzz.html#addition-madd-2008-s
func (p *{{ toLower .PointName }}JacExtended) add(a *{{ toUpper .PointName }}Affine) *{{ toLower .PointName }}JacExtended {
	{{ template "add" dict "all" . "negate" false}}
}

// doubleNeg same as double, but will negate q.Y
func (p *{{ toLower .PointName }}JacExtended) doubleNeg(q *{{ toUpper .PointName }}Affine) *{{ toLower .PointName }}JacExtended {
	{{ template "mDouble" dict "all" . "negate" true}}
}


// double point in ZZ coords
// http://www.hyperelliptic.org/EFD/ {{ toLower .PointName }}p/auto-shortw-xyzz.html#doubling-dbl-2008-s-1
func (p *{{ toLower .PointName }}JacExtended) double(q *{{ toUpper .PointName }}Affine) *{{ toLower .PointName }}JacExtended {
	{{ template "mDouble" dict "all" . "negate" false}}
}


{{define "mDouble" }}
	var U, S, M, _M, Y3 {{.all.CoordType}}

	U.Double(&q.Y)
	{{if .negate}}
		U.Neg(&U)
	{{end}}
	p.ZZ.Square(&U)
	p.ZZZ.Mul(&U, &p.ZZ)
	S.Mul(&q.X, &p.ZZ)
	_M.Square(&q.X)
	M.Double(&_M).
		Add(&M, &_M) // -> + a, but a=0 here
	p.X.Square(&M).
		Sub(&p.X, &S).
		Sub(&p.X, &S)
	Y3.Sub(&S, &p.X).Mul(&Y3, &M)
	U.Mul(&p.ZZZ, &q.Y)
	{{if .negate}}
		p.Y.Add(&Y3, &U)
	{{else}}
		p.Y.Sub(&Y3, &U)
	{{end}}

	return p
{{end}}

{{define "add" }}
	//if a is infinity return p
	if a.X.IsZero() && a.Y.IsZero() {
		return p
	}
	// p is infinity, return a
	if p.ZZ.IsZero() {
		p.X = a.X
		p.Y = a.Y
		{{if .negate}}
		p.Y.Neg(&p.Y)
		{{end}}
		p.ZZ.SetOne()
		p.ZZZ.SetOne()
		return p
	}

	var P, R {{.all.CoordType}} 

	// p2: a, p1: p
	P.Mul(&a.X, &p.ZZ)
	P.Sub(&P, &p.X)

	R.Mul(&a.Y, &p.ZZZ)
	{{if .negate}}
		R.Neg(&R)
	{{end}}
	R.Sub(&R, &p.Y)

	if P.IsZero() {
		if R.IsZero() {
			{{if .negate}}
				return p.doubleNeg(a)
			{{ else }}
				return p.double(a)
			{{end}}
			
		} 
		p.ZZ = {{.all.CoordType}}{}
		p.ZZZ = {{.all.CoordType}}{}
		return p
	} 

	var PP, PPP, Q, Q2, RR, X3, Y3 {{.all.CoordType}}


	PP.Square(&P)
	PPP.Mul(&P, &PP)
	Q.Mul(&p.X, &PP)
	RR.Square(&R)
	X3.Sub(&RR, &PPP)
	Q2.Double(&Q)
	p.X.Sub(&X3, &Q2)
	Y3.Sub(&Q, &p.X).Mul(&Y3, &R)
	R.Mul(&p.Y, &PPP)
	p.Y.Sub(&Y3, &R)
	p.ZZ.Mul(&p.ZZ, &PP)
	p.ZZZ.Mul(&p.ZZZ, &PPP)

	return p
{{ end }}

`

// MultiExpHelpers is common to both points (only one is generated per package)
const MultiExpHelpers = `

import (
	"github.com/consensys/gurvy/{{ toLower .CurveName}}/fr"
)

// CPUSemaphore enables users to set optional number of CPUs the multiexp will use
// this is thread safe and can be used accross parallel calls of gurvy.MultiExp 
type CPUSemaphore struct {
	chCpus chan struct{} // semaphore to limit number of cpus iterating through points and scalrs at the same time
	lock sync.Mutex 
}

// NewCPUSemaphore returns a new multiExp options to be used with MultiExp
// this option can be shared between different MultiExp calls and will ensure only numCpus are used
// through a semaphore
func NewCPUSemaphore(numCpus int) *CPUSemaphore {
	toReturn := &CPUSemaphore{
		chCpus: make(chan struct{}, numCpus),
	}
	for i:=0; i < numCpus; i++ {
		toReturn.chCpus <- struct{}{}
	}
	return toReturn 
}


// selector stores the index, mask and shifts needed to select bits from a scalar
// it is used during the multiExp algorithm or the batch scalar multiplication
type selector struct {
	index uint64 			// index in the multi-word scalar to select bits from
	mask uint64				// mask (c-bit wide) 
	shift uint64			// shift needed to get our bits on low positions

	multiWordSelect bool	// set to true if we need to select bits from 2 words (case where c doesn't divide 64)
	maskHigh uint64 	  	// same than mask, for index+1
	shiftHigh uint64		// same than shift, for index+1
}

// partitionScalars  compute, for each scalars over c-bit wide windows, nbChunk digits
// if the digit is larger than 2^{c-1}, then, we borrow 2^c from the next window and substract
// 2^{c} to the current digit, making it negative.
// negative digits can be processed in a later step as adding -G into the bucket instead of G
// (computing -G is cheap, and this saves us half of the buckets in the MultiExp or BatchScalarMul)
func partitionScalars(scalars []fr.Element, c uint64) []fr.Element {
	toReturn := make([]fr.Element, len(scalars))


	// number of c-bit radixes in a scalar
	nbChunks := fr.Limbs * 64 / c 
	if (fr.Limbs * 64)%c != 0 {
		nbChunks++
	}

	mask  := uint64((1 << c) - 1) 		// low c bits are 1
	msbWindow := uint64(1 << (c -1)) 			// msb of the c-bit window
	max := int(1 << (c -1)) 					// max value we want for our digits
	cDivides64 :=  (64 %c ) == 0 				// if c doesn't divide 64, we may need to select over multiple words
	

	// compute offset and word selector / shift to select the right bits of our windows
	selectors := make([]selector, nbChunks)
	for chunk:=uint64(0); chunk < nbChunks; chunk++ {
		jc := uint64(chunk * c)
		d := selector{}
		d.index = jc / 64
		d.shift = jc - (d.index * 64)
		d.mask = mask << d.shift
		d.multiWordSelect = !cDivides64  && d.shift > (64-c) && d.index < (fr.Limbs - 1 )
		if d.multiWordSelect {
			nbBitsHigh := d.shift - uint64(64-c)
			d.maskHigh = (1 << nbBitsHigh) - 1
			d.shiftHigh = (c - nbBitsHigh)
		}
		selectors[chunk] = d
	}


	parallel.Execute(len(scalars), func(start, end int) {
		for i:=start; i < end; i++ {
			var carry int

			// for each chunk in the scalar, compute the current digit, and an eventual carry
			for chunk := uint64(0); chunk < nbChunks; chunk++ {
				s := selectors[chunk]

				// init with carry if any
				digit := carry
				carry = 0

				// digit = value of the c-bit window
				digit += int((scalars[i][s.index] & s.mask) >> s.shift)
				
				if s.multiWordSelect {
					// we are selecting bits over 2 words
					digit += int(scalars[i][s.index+1] & s.maskHigh) << s.shiftHigh
				}

				// if the digit is larger than 2^{c-1}, then, we borrow 2^c from the next window and substract
				// 2^{c} to the current digit, making it negative.
				if digit >= max {
					digit -= (1 << c)
					carry = 1
				}

				var bits uint64
				if digit >= 0 {
					bits = uint64(digit)
				} else {
					bits = uint64(-digit-1) | msbWindow
				}
				
				toReturn[i][s.index] |= (bits << s.shift)
				if s.multiWordSelect {
					toReturn[i][s.index+1] |= (bits >> s.shiftHigh)
				}
				
			}
		}
	})
	return toReturn
}

`
