// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/types/json"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/mock"
	"github.com/pingcap/tidb/util/testleak"
)

type mockVecPlusIntBuiltinFunc struct {
	baseBuiltinFunc

	buf         *chunk.Column
	enableAlloc bool
}

func (p *mockVecPlusIntBuiltinFunc) allocBuf(n int) (*chunk.Column, error) {
	if p.enableAlloc {
		return p.get(types.ETInt, n)
	}
	if p.buf == nil {
		p.buf = chunk.NewColumn(types.NewFieldType(mysql.TypeLonglong), n)
	}
	return p.buf, nil
}

func (p *mockVecPlusIntBuiltinFunc) releaseBuf(buf *chunk.Column) {
	if p.enableAlloc {
		p.put(buf)
	}
}

func (p *mockVecPlusIntBuiltinFunc) vecEval(input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf, err := p.allocBuf(n)
	if err != nil {
		return err
	}
	defer p.releaseBuf(buf)
	if err := p.args[0].VecEval(p.ctx, input, result); err != nil {
		return err
	}
	if err := p.args[1].VecEval(p.ctx, input, buf); err != nil {
		return err
	}
	dst64s := result.Int64s()
	src64s := buf.Int64s()
	for i := range dst64s {
		dst64s[i] += src64s[i]
	}
	for i := 0; i < n; i++ {
		if buf.IsNull(i) && !result.IsNull(i) {
			result.SetNull(i, true)
		}
	}
	return nil
}

func genMockVecPlusIntBuiltinFunc() (*mockVecPlusIntBuiltinFunc, *chunk.Chunk, *chunk.Column) {
	tp := types.NewFieldType(mysql.TypeLonglong)
	col1 := newColumn(0)
	col1.Index, col1.RetType = 0, tp
	col2 := newColumn(1)
	col2.Index, col2.RetType = 1, tp
	bf := newBaseBuiltinFuncWithTp(mock.NewContext(), []Expression{col1, col2}, types.ETInt, types.ETInt, types.ETInt)
	plus := &mockVecPlusIntBuiltinFunc{bf, nil, false}
	input := chunk.New([]*types.FieldType{tp, tp}, 1024, 1024)
	buf := chunk.NewColumn(types.NewFieldType(mysql.TypeLonglong), 1024)
	for i := 0; i < 1024; i++ {
		input.AppendInt64(0, int64(i))
		input.AppendInt64(1, int64(i))
	}
	return plus, input, buf
}

func (s *testEvaluatorSuite) TestMockVecPlusInt(c *C) {
	plus, input, buf := genMockVecPlusIntBuiltinFunc()
	plus.enableAlloc = false
	c.Assert(plus.vecEval(input, buf), IsNil)
	for i := 0; i < 1024; i++ {
		c.Assert(buf.IsNull(i), IsFalse)
		c.Assert(buf.GetInt64(i), Equals, int64(i*2))
	}

	plus.enableAlloc = true
	c.Assert(plus.vecEval(input, buf), IsNil)
	for i := 0; i < 1024; i++ {
		c.Assert(buf.IsNull(i), IsFalse)
		c.Assert(buf.GetInt64(i), Equals, int64(i*2))
	}
}

func (s *testEvaluatorSuite) TestMockVecPlusIntParallel(c *C) {
	plus, input, buf := genMockVecPlusIntBuiltinFunc()
	plus.enableAlloc = true // it's concurrency-safe if enableAlloc is true
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			result := buf.CopyConstruct(nil)
			for i := 0; i < 200; i++ {
				c.Assert(plus.vecEval(input, result), IsNil)
				for i := 0; i < 1024; i++ {
					c.Assert(result.IsNull(i), IsFalse)
					c.Assert(result.GetInt64(i), Equals, int64(i*2))
				}
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

func BenchmarkColumnBufferAllocate(b *testing.B) {
	allocator := newLocalSliceBuffer(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, _ := allocator.get(types.ETInt, 1024)
		allocator.put(buf)
	}
}

func BenchmarkColumnBufferAllocateParallel(b *testing.B) {
	allocator := newLocalSliceBuffer(1)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf, _ := allocator.get(types.ETInt, 1024)
			allocator.put(buf)
		}
	})
}

func BenchmarkPlusIntBufAllocator(b *testing.B) {
	plus, input, buf := genMockVecPlusIntBuiltinFunc()
	names := []string{"enable", "disable"}
	enable := []bool{true, false}
	for i := range enable {
		b.Run(names[i], func(b *testing.B) {
			plus.enableAlloc = enable[i]
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := plus.vecEval(input, buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type mockRowBuiltinDouble struct {
	baseBuiltinFunc
}

func (p *mockRowBuiltinDouble) evalInt(row chunk.Row) (int64, bool, error) {
	v, isNull, err := p.args[0].EvalInt(p.ctx, row)
	if err != nil {
		return 0, false, err
	}
	return v * 2, isNull, nil
}

func (p *mockRowBuiltinDouble) evalReal(row chunk.Row) (float64, bool, error) {
	v, isNull, err := p.args[0].EvalReal(p.ctx, row)
	if err != nil {
		return 0, false, err
	}
	return v * 2, isNull, nil
}

func (p *mockRowBuiltinDouble) evalString(row chunk.Row) (string, bool, error) {
	v, isNull, err := p.args[0].EvalString(p.ctx, row)
	if err != nil {
		return "", false, err
	}
	return v + v, isNull, nil
}

func (p *mockRowBuiltinDouble) evalDecimal(row chunk.Row) (*types.MyDecimal, bool, error) {
	v, isNull, err := p.args[0].EvalDecimal(p.ctx, row)
	if err != nil {
		return nil, false, err
	}
	r := new(types.MyDecimal)
	if err := types.DecimalAdd(v, v, r); err != nil {
		return nil, false, err
	}
	return r, isNull, nil
}

func (p *mockRowBuiltinDouble) evalTime(row chunk.Row) (types.Time, bool, error) {
	v, isNull, err := p.args[0].EvalTime(p.ctx, row)
	if err != nil {
		return types.Time{}, false, err
	}
	d, err := v.ConvertToDuration()
	if err != nil {
		return types.Time{}, false, err
	}
	v, err = v.Add(p.ctx.GetSessionVars().StmtCtx, d)
	return v, isNull, err
}

func (p *mockRowBuiltinDouble) evalDuration(row chunk.Row) (types.Duration, bool, error) {
	v, isNull, err := p.args[0].EvalDuration(p.ctx, row)
	if err != nil {
		return types.Duration{}, false, err
	}
	v, err = v.Add(v)
	return v, isNull, err
}

func (p *mockRowBuiltinDouble) evalJSON(row chunk.Row) (json.BinaryJSON, bool, error) {
	j, isNull, err := p.args[0].EvalJSON(p.ctx, row)
	if err != nil {
		return json.BinaryJSON{}, false, err
	}
	if isNull {
		return json.BinaryJSON{}, true, nil
	}
	path, err := json.ParseJSONPathExpr("$.key")
	if err != nil {
		return json.BinaryJSON{}, false, err
	}
	ret, ok := j.Extract([]json.PathExpression{path})
	if !ok {
		return json.BinaryJSON{}, true, err
	}
	if err := j.UnmarshalJSON([]byte(fmt.Sprintf(`{"key":%v}`, 2*ret.GetInt64()))); err != nil {
		return json.BinaryJSON{}, false, err
	}
	return j, false, nil
}

func convertETType(eType types.EvalType) (mysqlType byte) {
	switch eType {
	case types.ETInt:
		mysqlType = mysql.TypeLonglong
	case types.ETReal:
		mysqlType = mysql.TypeDouble
	case types.ETDecimal:
		mysqlType = mysql.TypeNewDecimal
	case types.ETDuration:
		mysqlType = mysql.TypeDuration
	case types.ETJson:
		mysqlType = mysql.TypeJSON
	case types.ETString:
		mysqlType = mysql.TypeVarString
	case types.ETDatetime:
		mysqlType = mysql.TypeDatetime
	}
	return
}

func genMockRowDouble(eType types.EvalType) (builtinFunc, *chunk.Chunk, *chunk.Column, error) {
	mysqlType := convertETType(eType)
	tp := types.NewFieldType(mysqlType)
	col1 := newColumn(1)
	col1.Index = 0
	col1.RetType = tp
	bf := newBaseBuiltinFuncWithTp(mock.NewContext(), []Expression{col1}, eType, eType)
	rowDouble := &mockRowBuiltinDouble{bf}
	input := chunk.New([]*types.FieldType{tp}, 1024, 1024)
	buf := chunk.NewColumn(types.NewFieldType(convertETType(eType)), 1024)
	for i := 0; i < 1024; i++ {
		switch eType {
		case types.ETInt:
			input.AppendInt64(0, int64(i))
		case types.ETReal:
			input.AppendFloat64(0, float64(i))
		case types.ETDecimal:
			dec := new(types.MyDecimal)
			if err := dec.FromFloat64(float64(i)); err != nil {
				return nil, nil, nil, err
			}
			input.AppendMyDecimal(0, dec)
		case types.ETDuration:
			input.AppendDuration(0, types.Duration{Duration: time.Duration(i)})
		case types.ETJson:
			j := new(json.BinaryJSON)
			if err := j.UnmarshalJSON([]byte(fmt.Sprintf(`{"key":%v}`, i))); err != nil {
				return nil, nil, nil, err
			}
			input.AppendJSON(0, *j)
		case types.ETString:
			input.AppendString(0, fmt.Sprintf("%v", i))
		case types.ETDatetime:
			t := types.FromDate(i, 0, 0, 0, 0, 0, 0)
			input.AppendTime(0, types.Time{Time: t, Type: mysqlType})
		}
	}
	return &vecRowConverter{rowDouble}, input, buf, nil
}

func (s *testEvaluatorSuite) checkVecEval(c *C, eType types.EvalType, sel []int, result *chunk.Column) {
	if sel == nil {
		for i := 0; i < 1024; i++ {
			sel = append(sel, i)
		}
	}
	switch eType {
	case types.ETInt:
		i64s := result.Int64s()
		c.Assert(len(i64s), Equals, len(sel))
		for i, j := range sel {
			c.Assert(i64s[i], Equals, int64(j*2))
		}
	case types.ETReal:
		f64s := result.Float64s()
		c.Assert(len(f64s), Equals, len(sel))
		for i, j := range sel {
			c.Assert(f64s[i], Equals, float64(j*2))
		}
	case types.ETDecimal:
		ds := result.Decimals()
		c.Assert(len(ds), Equals, len(sel))
		for i, j := range sel {
			dec := new(types.MyDecimal)
			c.Assert(dec.FromFloat64(float64(j)), IsNil)
			rst := new(types.MyDecimal)
			c.Assert(types.DecimalAdd(dec, dec, rst), IsNil)
			c.Assert(rst.Compare(&ds[i]), Equals, 0)
		}
	case types.ETDuration:
		ds := result.GoDurations()
		c.Assert(len(ds), Equals, len(sel))
		for i, j := range sel {
			c.Assert(ds[i], Equals, time.Duration(j+j))
		}
	case types.ETDatetime:
		ds := result.Times()
		c.Assert(len(ds), Equals, len(sel))
		for i, j := range sel {
			gt := types.FromDate(j, 0, 0, 0, 0, 0, 0)
			t := types.Time{Time: gt, Type: convertETType(eType)}
			d, err := t.ConvertToDuration()
			c.Assert(err, IsNil)
			v, err := t.Add(mock.NewContext().GetSessionVars().StmtCtx, d)
			c.Assert(err, IsNil)
			c.Assert(v.Compare(ds[i]), Equals, 0)
		}
	case types.ETJson:
		for i, j := range sel {
			path, err := json.ParseJSONPathExpr("$.key")
			c.Assert(err, IsNil)
			ret, ok := result.GetJSON(i).Extract([]json.PathExpression{path})
			c.Assert(ok, IsTrue)
			c.Assert(ret.GetInt64(), Equals, int64(j*2))
		}
	case types.ETString:
		for i, j := range sel {
			c.Assert(result.GetString(i), Equals, fmt.Sprintf("%v%v", j, j))
		}
	}
}

func (s *testEvaluatorSuite) TestDoubleRow2Vec(c *C) {
	defer testleak.AfterTest(c)()
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for _, eType := range eTypes {
		rowDouble, input, result, err := genMockRowDouble(eType)
		c.Assert(err, IsNil)
		c.Assert(rowDouble.vecEval(input, result), IsNil)
		s.checkVecEval(c, eType, nil, result)

		sel := []int{0}
		for {
			end := sel[len(sel)-1]
			gap := 1024 - end
			if gap < 10 {
				break
			}
			sel = append(sel, end+rand.Intn(gap-1)+1)
		}
		input.SetSel(sel)
		c.Assert(rowDouble.vecEval(input, result), IsNil)

		s.checkVecEval(c, eType, sel, result)
	}
}

func BenchmarkMockDoubleRow(b *testing.B) {
	typeNames := []string{"Int", "Real", "Decimal", "Duration", "String", "Datetime", "JSON"}
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for i, eType := range eTypes {
		b.Run(typeNames[i], func(b *testing.B) {
			rowDouble, input, result, _ := genMockRowDouble(eType)
			it := chunk.NewIterator4Chunk(input)
			b.ResetTimer()
			switch eType {
			case types.ETInt:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalInt(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendInt64(v)
						}
					}
				}
			case types.ETReal:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalReal(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendFloat64(v)
						}
					}
				}
			case types.ETDecimal:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalDecimal(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendMyDecimal(v)
						}
					}
				}
			case types.ETDuration:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalDuration(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendDuration(v)
						}
					}
				}
			case types.ETString:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalString(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendString(v)
						}
					}
				}
			case types.ETDatetime:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalTime(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendTime(v)
						}
					}
				}
			case types.ETJson:
				for i := 0; i < b.N; i++ {
					result.Reset()
					for r := it.Begin(); r != it.End(); r = it.Next() {
						v, isNull, err := rowDouble.evalJSON(r)
						if err != nil {
							b.Fatal(err)
						}
						if isNull {
							result.AppendNull()
						} else {
							result.AppendJSON(v)
						}
					}
				}
			}
		})
	}
}

func BenchmarkMockDoubleRow2Vec(b *testing.B) {
	typeNames := []string{"Int", "Real", "Decimal", "Duration", "String", "Datetime", "JSON"}
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for i, eType := range eTypes {
		b.Run(typeNames[i], func(b *testing.B) {
			rowDouble, input, result, _ := genMockRowDouble(eType)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := rowDouble.vecEval(input, result)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
