// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package groth16

import (
	"github.com/consensys/gurvy/bls381/fr"

	curve "github.com/consensys/gurvy/bls381"

	bls381backend "github.com/consensys/gnark/internal/backend/bls381/cs"

	"github.com/consensys/gnark/internal/backend/bls381/fft"

	bls381witness "github.com/consensys/gnark/internal/backend/bls381/witness"

	"fmt"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gurvy"
	"math/big"
	"runtime"
)

// Proof represents a Groth16 proof that was encoded with a ProvingKey and can be verified
// with a valid statement and a VerifyingKey
// Notation follows Figure 4. in DIZK paper https://eprint.iacr.org/2018/691.pdf
type Proof struct {
	Ar, Krs curve.G1Affine
	Bs      curve.G2Affine
}

// isValid ensures proof elements are in the correct subgroup
func (proof *Proof) isValid() bool {
	return proof.Ar.IsInSubGroup() && proof.Krs.IsInSubGroup() && proof.Bs.IsInSubGroup()
}

// GetCurveID returns the curveID
func (proof *Proof) GetCurveID() gurvy.ID {
	return curve.ID
}

// Prove generates the proof of knoweldge of a r1cs with full witness (secret + public part).
// if force flag is set, Prove ignores R1CS solving error (ie invalid witness) and executes
// the FFTs and MultiExponentiations to compute an (invalid) Proof object
func Prove(r1cs *bls381backend.R1CS, pk *ProvingKey, witness bls381witness.Witness, force bool) (*Proof, error) {
	if len(witness) != int(r1cs.NbPublicVariables-1+r1cs.NbSecretVariables) {
		return nil, fmt.Errorf("invalid witness size, got %d, expected %d = %d (public - ONE_WIRE) + %d (secret)", len(witness), int(r1cs.NbPublicVariables-1+r1cs.NbSecretVariables), r1cs.NbPublicVariables, r1cs.NbSecretVariables)
	}

	// solve the R1CS and compute the a, b, c vectors
	a := make([]fr.Element, r1cs.NbConstraints, pk.Domain.Cardinality)
	b := make([]fr.Element, r1cs.NbConstraints, pk.Domain.Cardinality)
	c := make([]fr.Element, r1cs.NbConstraints, pk.Domain.Cardinality)
	wireValues := make([]fr.Element, r1cs.NbInternalVariables+r1cs.NbPublicVariables+r1cs.NbSecretVariables)
	if err := r1cs.Solve(witness, a, b, c, wireValues); err != nil && !force {
		return nil, err
	}

	// set the wire values in regular form
	utils.Parallelize(len(wireValues), func(start, end int) {
		for i := start; i < end; i++ {
			wireValues[i].FromMont()
		}
	})

	// H (witness reduction / FFT part)
	var h []fr.Element
	chHDone := make(chan struct{}, 1)
	go func() {
		h = computeH(a, b, c, &pk.Domain)
		a = nil
		b = nil
		c = nil
		chHDone <- struct{}{}
	}()

	// sample random r and s
	var r, s big.Int
	var _r, _s, _kr fr.Element
	if _, err := _r.SetRandom(); err != nil {
		return nil, err
	}
	if _, err := _s.SetRandom(); err != nil {
		return nil, err
	}
	_kr.Mul(&_r, &_s).Neg(&_kr)

	_r.FromMont()
	_s.FromMont()
	_kr.FromMont()
	_r.ToBigInt(&r)
	_s.ToBigInt(&s)

	// computes r[δ], s[δ], kr[δ]
	deltas := curve.BatchScalarMultiplicationG1(&pk.G1.Delta, []fr.Element{_r, _s, _kr})

	proof := &Proof{}
	var bs1, ar curve.G1Jac

	// using this ensures that our multiExps running in parallel won't use more than
	// provided CPUs
	cpuSemaphore := curve.NewCPUSemaphore(runtime.NumCPU())

	chBs1Done := make(chan struct{}, 1)
	computeBS1 := func() {
		bs1.MultiExp(pk.G1.B, wireValues, cpuSemaphore)
		bs1.AddMixed(&pk.G1.Beta)
		bs1.AddMixed(&deltas[1])
		chBs1Done <- struct{}{}
	}

	chArDone := make(chan struct{}, 1)
	computeAR1 := func() {
		ar.MultiExp(pk.G1.A, wireValues, cpuSemaphore)
		ar.AddMixed(&pk.G1.Alpha)
		ar.AddMixed(&deltas[0])
		proof.Ar.FromJacobian(&ar)
		chArDone <- struct{}{}
	}

	chKrsDone := make(chan struct{}, 1)
	computeKRS := func() {
		// we could NOT split the Krs multiExp in 2, and just append pk.G1.K and pk.G1.Z
		// however, having similar lengths for our tasks helps with parallelism

		var krs, krs2, p1 curve.G1Jac
		chKrs2Done := make(chan struct{}, 1)
		go func() {
			krs2.MultiExp(pk.G1.Z, h, cpuSemaphore)
			chKrs2Done <- struct{}{}
		}()
		krs.MultiExp(pk.G1.K, wireValues[r1cs.NbPublicVariables:], cpuSemaphore)
		krs.AddMixed(&deltas[2])
		n := 3
		for n != 0 {
			select {
			case <-chKrs2Done:
				krs.AddAssign(&krs2)
			case <-chArDone:
				p1.ScalarMultiplication(&ar, &s)
				krs.AddAssign(&p1)
			case <-chBs1Done:
				p1.ScalarMultiplication(&bs1, &r)
				krs.AddAssign(&p1)
			}
			n--
		}

		proof.Krs.FromJacobian(&krs)
		chKrsDone <- struct{}{}
	}

	computeBS2 := func() {
		// Bs2 (1 multi exp G2 - size = len(wires))
		var Bs, deltaS curve.G2Jac

		// splitting Bs2 in 3 ensures all our go routines in the prover have similar running time
		// and is good for parallelism. However, on a machine with limited CPUs, this may not be
		// a good idea, as the MultiExp scales slightly better than linearly
		bsSplit := len(pk.G2.B) / 3
		if bsSplit > 10 {
			chDone1 := make(chan struct{}, 1)
			chDone2 := make(chan struct{}, 1)
			var bs1, bs2 curve.G2Jac
			go func() {
				bs1.MultiExp(pk.G2.B[:bsSplit], wireValues[:bsSplit], cpuSemaphore)
				chDone1 <- struct{}{}
			}()
			go func() {
				bs2.MultiExp(pk.G2.B[bsSplit:bsSplit*2], wireValues[bsSplit:bsSplit*2], cpuSemaphore)
				chDone2 <- struct{}{}
			}()
			Bs.MultiExp(pk.G2.B[bsSplit*2:], wireValues[bsSplit*2:], cpuSemaphore)

			<-chDone1
			Bs.AddAssign(&bs1)
			<-chDone2
			Bs.AddAssign(&bs2)
		} else {
			Bs.MultiExp(pk.G2.B, wireValues, cpuSemaphore)
		}

		deltaS.FromAffine(&pk.G2.Delta)
		deltaS.ScalarMultiplication(&deltaS, &s)
		Bs.AddAssign(&deltaS)
		Bs.AddMixed(&pk.G2.Beta)

		proof.Bs.FromJacobian(&Bs)
	}

	// wait for FFT to end, as it uses all our CPUs
	<-chHDone

	// schedule our proof part computations
	go computeKRS()
	go computeAR1()
	go computeBS1()
	computeBS2()

	// wait for all parts of the proof to be computed.
	<-chKrsDone

	return proof, nil
}

func computeH(a, b, c []fr.Element, domain *fft.Domain) []fr.Element {
	// H part of Krs
	// Compute H (hz=ab-c, where z=-2 on ker X^n+1 (z(x)=x^n-1))
	// 	1 - _a = ifft(a), _b = ifft(b), _c = ifft(c)
	// 	2 - ca = fft_coset(_a), ba = fft_coset(_b), cc = fft_coset(_c)
	// 	3 - h = ifft_coset(ca o cb - cc)

	n := len(a)

	// add padding to ensure input length is domain cardinality
	padding := make([]fr.Element, int(domain.Cardinality)-n)
	a = append(a, padding...)
	b = append(b, padding...)
	c = append(c, padding...)
	n = len(a)

	domain.FFTInverse(a, fft.DIF, 0)
	domain.FFTInverse(b, fft.DIF, 0)
	domain.FFTInverse(c, fft.DIF, 0)

	// utils.Parallelize(n, func(start, end int) {
	// 	for i := start; i < end; i++ {
	// 		a[i].Mul(&a[i], &domain.CosetTable[i])
	// 		b[i].Mul(&b[i], &domain.CosetTable[i])
	// 		c[i].Mul(&c[i], &domain.CosetTable[i])
	// 	}
	// })

	domain.FFT(a, fft.DIT, 1)
	domain.FFT(b, fft.DIT, 1)
	domain.FFT(c, fft.DIT, 1)

	var minusTwoInv fr.Element
	minusTwoInv.SetUint64(2)
	minusTwoInv.Neg(&minusTwoInv).
		Inverse(&minusTwoInv)

	// h = ifft_coset(ca o cb - cc)
	// reusing a to avoid unecessary memalloc
	utils.Parallelize(n, func(start, end int) {
		for i := start; i < end; i++ {
			a[i].Mul(&a[i], &b[i]).
				Sub(&a[i], &c[i]).
				Mul(&a[i], &minusTwoInv)
		}
	})

	// ifft_coset
	domain.FFTInverse(a, fft.DIF, 1)

	utils.Parallelize(len(a), func(start, end int) {
		for i := start; i < end; i++ {
			a[i].FromMont()
		}
	})
	// utils.Parallelize(n, func(start, end int) {
	// 	for i := start; i < end; i++ {
	// 		a[i].Mul(&a[i], &domain.CosetTableInv[i]).FromMont()
	// 	}
	// })

	return a
}
