package oci8

// #include "oci8.go.h"
import "C"

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"time"
	"unsafe"
)

// Close closes the statment
func (stmt *OCI8Stmt) Close() error {
	if stmt.closed {
		return nil
	}
	stmt.closed = true

	C.OCIHandleFree(unsafe.Pointer(stmt.stmt), C.OCI_HTYPE_STMT)

	stmt.stmt = nil
	stmt.pbind = nil

	return nil
}

// NumInput returns the number of input
func (stmt *OCI8Stmt) NumInput() int {
	r := C.WrapOCIAttrGetInt(unsafe.Pointer(stmt.stmt), C.OCI_HTYPE_STMT, C.OCI_ATTR_BIND_COUNT, stmt.conn.errHandle)
	if r.rv != C.OCI_SUCCESS {
		return -1
	}
	return int(r.num)
}

// bind binds the varables / arguments
func (stmt *OCI8Stmt) bind(args []namedValue) ([]oci8bind, error) {
	if len(args) == 0 {
		return nil, nil
	}

	var boundParameters []oci8bind
	var err error

	for i, uv := range args {
		var sbind oci8bind

		vv := uv.Value
		if out, ok := handleOutput(vv); ok {
			sbind.out = out.Dest
			vv, err = driver.DefaultParameterConverter.ConvertValue(out.Dest)
			if err != nil {
				defer freeBoundParameters(boundParameters)
				return nil, err
			}
		}

		switch v := vv.(type) {

		case nil:
			sbind.kind = C.SQLT_STR
			sbind.clen = 0
			sbind.pbuf = nil
			sbind.indicator = -1 // set to null

		case []byte:
			sbind.kind = C.SQLT_BIN
			sbind.clen = C.sb4(len(v))
			sbind.pbuf = unsafe.Pointer(CByte(v))

		case time.Time:

			var pt unsafe.Pointer
			var zp unsafe.Pointer

			zone, offset := v.Zone()

			size := len(zone)
			if size < 8 {
				size = 8
			}
			size += int(sizeOfNilPointer)
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_TIMESTAMP_TZ,
				C.size_t(size),
			); ret.rv != C.OCI_SUCCESS {
				defer freeBoundParameters(boundParameters)
				return nil, stmt.conn.getError(ret.rv)
			} else {
				sbind.kind = C.SQLT_TIMESTAMP_TZ
				sbind.clen = C.sb4(unsafe.Sizeof(pt))
				pt = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
				zp = unsafe.Pointer(uintptr(ret.extra) + sizeOfNilPointer)
			}

			tryagain := false

			copy((*[1 << 30]byte)(zp)[0:len(zone)], zone)
			rv := C.OCIDateTimeConstruct(
				unsafe.Pointer(stmt.conn.env),
				stmt.conn.errHandle,
				(*C.OCIDateTime)(*(*unsafe.Pointer)(pt)),
				C.sb2(v.Year()),
				C.ub1(v.Month()),
				C.ub1(v.Day()),
				C.ub1(v.Hour()),
				C.ub1(v.Minute()),
				C.ub1(v.Second()),
				C.ub4(v.Nanosecond()),
				(*C.OraText)(zp),
				C.size_t(len(zone)),
			)
			if rv != C.OCI_SUCCESS {
				tryagain = true
			} else {
				//check if oracle timezone offset is same ?
				rvz := C.WrapOCIDateTimeGetTimeZoneNameOffset(
					stmt.conn.env,
					stmt.conn.errHandle,
					(*C.OCIDateTime)(*(*unsafe.Pointer)(pt)))
				if rvz.rv != C.OCI_SUCCESS {
					defer freeBoundParameters(boundParameters)
					return nil, stmt.conn.getError(rvz.rv)
				}
				if offset != int(rvz.h)*60*60+int(rvz.m)*60 {
					//fmt.Println("oracle timezone offset dont match", zone, offset, int(rvz.h)*60*60+int(rvz.m)*60)
					tryagain = true
				}
			}

			if tryagain {
				sign := '+'
				if offset < 0 {
					offset = -offset
					sign = '-'
				}
				offset /= 60
				// oracle accept zones "[+-]hh:mm", try second time
				zone = fmt.Sprintf("%c%02d:%02d", sign, offset/60, offset%60)

				copy((*[1 << 30]byte)(zp)[0:len(zone)], zone)
				rv := C.OCIDateTimeConstruct(
					unsafe.Pointer(stmt.conn.env),
					stmt.conn.errHandle,
					(*C.OCIDateTime)(*(*unsafe.Pointer)(pt)),
					C.sb2(v.Year()),
					C.ub1(v.Month()),
					C.ub1(v.Day()),
					C.ub1(v.Hour()),
					C.ub1(v.Minute()),
					C.ub1(v.Second()),
					C.ub4(v.Nanosecond()),
					(*C.OraText)(zp),
					C.size_t(len(zone)),
				)
				if rv != C.OCI_SUCCESS {
					defer freeBoundParameters(boundParameters)
					return nil, stmt.conn.getError(rv)
				}
			}

			sbind.pbuf = unsafe.Pointer((*C.char)(pt))

		case string:
			if sbind.out != nil {
				sbind.kind = C.SQLT_STR
				sbind.clen = C.sb4(len(v) + 1)
				sbind.pbuf = unsafe.Pointer(C.CString(v))
			} else {
				sbind.kind = C.SQLT_AFC
				sbind.clen = C.sb4(len(v))
				sbind.pbuf = unsafe.Pointer(C.CString(v))
			}
			if len(v) == 0 {
				sbind.indicator = -1 // set to null
			}

		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
			buffer := bytes.Buffer{}
			err = binary.Write(&buffer, binary.LittleEndian, v)
			if err != nil {
				return nil, fmt.Errorf("binary read for column %v - error: %v", i, err)
			}
			sbind.kind = C.SQLT_INT
			sbind.clen = C.sb4(buffer.Len())
			sbind.pbuf = unsafe.Pointer(CByte(buffer.Bytes()))

		case float32, float64:
			buffer := bytes.Buffer{}
			err = binary.Write(&buffer, binary.LittleEndian, v)
			if err != nil {
				return nil, fmt.Errorf("binary read for column %v - error: %v", i, err)
			}
			sbind.kind = C.SQLT_BDOUBLE
			sbind.clen = C.sb4(buffer.Len())
			sbind.pbuf = unsafe.Pointer(CByte(buffer.Bytes()))

		case bool: // oracle does not have bool, handle as 0/1 int
			sbind.kind = C.SQLT_INT
			sbind.clen = C.sb4(1)
			if v {
				sbind.pbuf = unsafe.Pointer(CByte([]byte{1}))
			} else {
				sbind.pbuf = unsafe.Pointer(CByte([]byte{0}))
			}

		default:
			if sbind.out != nil {
				sbind.kind = C.SQLT_STR
				sbind.clen = 0
				sbind.pbuf = nil
				sbind.indicator = -1 // set to null
				// TODO: should this error instead of setting to null?
			} else {
				sbind.kind = C.SQLT_CHR
				d := fmt.Sprintf("%v", v)
				sbind.clen = C.sb4(len(d))
				sbind.pbuf = unsafe.Pointer(C.CString(d))
			}
		}

		// buffer has been set, add to boundParameters now so if error will be freed by freeBoundParameters call
		boundParameters = append(boundParameters, sbind)

		if uv.Name != "" {
			err = stmt.ociBindByName(sbind.bindHandle, []byte(":"+uv.Name), sbind.pbuf, sbind.clen, sbind.kind, sbind.indicator)
		} else {
			err = stmt.ociBindByPos(sbind.bindHandle, C.ub4(i+1), sbind.pbuf, sbind.clen, sbind.kind, sbind.indicator)
		}
		if err != nil {
			defer freeBoundParameters(stmt.pbind)
			return nil, err
		}

	}

	return boundParameters, nil
}

// Query runs a query
func (stmt *OCI8Stmt) Query(args []driver.Value) (rows driver.Rows, err error) {
	list := make([]namedValue, len(args))
	for i, v := range args {
		list[i] = namedValue{
			Ordinal: i + 1,
			Value:   v,
		}
	}
	return stmt.query(context.Background(), list, false)
}

func (stmt *OCI8Stmt) query(ctx context.Context, args []namedValue, closeRows bool) (driver.Rows, error) {
	var fbp []oci8bind
	var err error

	if fbp, err = stmt.bind(args); err != nil {
		return nil, err
	}

	defer freeBoundParameters(fbp)

	var stmtType C.ub2
	_, err = stmt.ociAttrGet(unsafe.Pointer(&stmtType), C.OCI_ATTR_STMT_TYPE)
	if err != nil {
		return nil, err
	}

	iter := C.ub4(1)
	if stmtType == C.OCI_STMT_SELECT {
		iter = 0
	}

	// set the row prefetch.  Only one extra row per fetch will be returned unless this is set.
	if stmt.conn.prefetchRows > 0 {
		if rv := C.WrapOCIAttrSetUb4(
			unsafe.Pointer(stmt.stmt),
			C.OCI_HTYPE_STMT,
			C.ub4(stmt.conn.prefetchRows),
			C.OCI_ATTR_PREFETCH_ROWS,
			stmt.conn.errHandle,
		); rv != C.OCI_SUCCESS {
			return nil, stmt.conn.getError(rv)
		}
	}

	// if non-zero, oci will fetch rows until the memory limit or row prefetch limit is hit.
	// useful for memory constrained systems
	if stmt.conn.prefetchMemory > 0 {
		if rv := C.WrapOCIAttrSetUb4(
			unsafe.Pointer(stmt.stmt),
			C.OCI_HTYPE_STMT,
			C.ub4(stmt.conn.prefetchMemory),
			C.OCI_ATTR_PREFETCH_MEMORY,
			stmt.conn.errHandle,
		); rv != C.OCI_SUCCESS {
			return nil, stmt.conn.getError(rv)
		}
	}

	mode := C.ub4(C.OCI_DEFAULT)
	if !stmt.conn.inTransaction {
		mode = mode | C.OCI_COMMIT_ON_SUCCESS
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			// select again to avoid race condition if both are done
			select {
			case <-done:
			default:
				C.OCIBreak(
					unsafe.Pointer(stmt.conn.svc),
					stmt.conn.errHandle)
			}

		}
	}()
	rv := C.OCIStmtExecute(
		stmt.conn.svc,
		stmt.stmt,
		stmt.conn.errHandle,
		iter,
		0,
		nil,
		nil,
		mode)
	close(done)
	if rv != C.OCI_SUCCESS {
		return nil, stmt.conn.getError(rv)
	}

	var paramCountUb4 C.ub4
	_, err = stmt.ociAttrGet(unsafe.Pointer(&paramCountUb4), C.OCI_ATTR_PARAM_COUNT)
	if err != nil {
		return nil, err
	}
	paramCount := int(paramCountUb4)

	oci8cols := make([]oci8col, paramCount)
	indrlenptr := C.calloc(C.size_t(paramCount), C.sizeof_indrlen)
	indrlen := (*[1 << 16]C.indrlen)(indrlenptr)[0:paramCount]
	for i := 0; i < paramCount; i++ {
		var param *C.OCIParam
		param, err = stmt.ociParamGet(C.ub4(i + 1))
		if err != nil {
			C.free(indrlenptr)
			return nil, err
		}
		defer C.OCIDescriptorFree(unsafe.Pointer(param), C.OCI_DTYPE_PARAM)

		var dataType C.ub2 // external datatype of the column. Valid datatypes like: SQLT_CHR, SQLT_DATE, SQLT_TIMESTAMP, etc.
		_, err = stmt.conn.ociAttrGet(param, unsafe.Pointer(&dataType), C.OCI_ATTR_DATA_TYPE)
		if err != nil {
			C.free(indrlenptr)
			return nil, err
		}

		if nsr := C.WrapOCIAttrGetString(
			unsafe.Pointer(param),
			C.OCI_DTYPE_PARAM,
			C.OCI_ATTR_NAME,
			stmt.conn.errHandle,
		); nsr.rv != C.OCI_SUCCESS {
			C.free(indrlenptr)
			return nil, stmt.conn.getError(nsr.rv)
		} else {
			// the name of the column that is being loaded.
			oci8cols[i].name = string((*[1 << 30]byte)(unsafe.Pointer(nsr.ptr))[0:int(nsr.size)])
		}

		var dataSize C.ub4 // Maximum size in bytes of the external data for the column. This can affect conversion buffer sizes.
		_, err = stmt.conn.ociAttrGet(param, unsafe.Pointer(&dataSize), C.OCI_ATTR_DATA_SIZE)
		if err != nil {
			C.free(indrlenptr)
			return nil, err
		}

		*stmt.defp = nil

		// switch on dataType
		switch dataType {

		case C.SQLT_CHR, C.SQLT_AFC, C.SQLT_VCS, C.SQLT_AVC:
			oci8cols[i].kind = C.SQLT_CHR
			oci8cols[i].size = int(dataSize) * 4 // utf8 enc
			oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size) + 1)

		case C.SQLT_BIN:
			oci8cols[i].kind = C.SQLT_BIN
			oci8cols[i].size = int(dataSize)
			oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))

		case C.SQLT_NUM:
			var precision int
			var scale int
			if rv := C.WrapOCIAttrGetInt(
				unsafe.Pointer(param),
				C.OCI_DTYPE_PARAM,
				C.OCI_ATTR_PRECISION,
				stmt.conn.errHandle,
			); rv.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(rv.rv)
			} else {
				// The precision of numeric type attributes.
				precision = int(rv.num)
			}
			if rv := C.WrapOCIAttrGetInt(
				unsafe.Pointer(param),
				C.OCI_DTYPE_PARAM,
				C.OCI_ATTR_SCALE,
				stmt.conn.errHandle,
			); rv.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(rv.rv)
			} else {
				// The scale of numeric type attributes.
				scale = int(rv.num)
			}
			// The precision of numeric type attributes. If the precision is nonzero and scale is -127, then it is a FLOAT;
			// otherwise, it is a NUMBER(precision, scale).
			// When precision is 0, NUMBER(precision, scale) can be represented simply as NUMBER.
			// https://docs.oracle.com/cd/E11882_01/appdev.112/e10646/oci06des.htm#LNOCI16458

			if (precision == 0 && scale == 0) || scale > 0 || scale == -127 {
				oci8cols[i].kind = C.SQLT_BDOUBLE
				oci8cols[i].size = 8
				oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))
			} else {
				oci8cols[i].kind = C.SQLT_INT
				oci8cols[i].size = 8
				oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))
			}

		case C.SQLT_INT:
			oci8cols[i].kind = C.SQLT_INT
			oci8cols[i].size = 8
			oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))

		case C.SQLT_BFLOAT, C.SQLT_IBFLOAT, C.SQLT_BDOUBLE, C.SQLT_IBDOUBLE:
			oci8cols[i].kind = C.SQLT_BDOUBLE
			oci8cols[i].size = 8
			oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))

		case C.SQLT_LNG:
			oci8cols[i].kind = C.SQLT_BIN
			oci8cols[i].size = 2000
			oci8cols[i].pbuf = C.malloc(C.size_t(oci8cols[i].size))

		case C.SQLT_CLOB, C.SQLT_BLOB:
			// allocate + io buffers + ub4
			size := int(unsafe.Sizeof(unsafe.Pointer(nil)) + unsafe.Sizeof(C.ub4(0)))
			if oci8cols[i].size < lobBufferSize {
				size += lobBufferSize
			} else {
				size += oci8cols[i].size
			}
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_LOB,
				C.size_t(size),
			); ret.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(ret.rv)
			} else {
				oci8cols[i].kind = dataType
				oci8cols[i].size = int(sizeOfNilPointer)
				oci8cols[i].pbuf = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
			}

			//      testing
			//		case C.SQLT_DAT:
			//
			//			oci8cols[i].kind = C.SQLT_DAT
			//			oci8cols[i].size = int(dataSize)
			//			oci8cols[i].pbuf = C.malloc(C.size_t(dataSize))
			//

		case C.SQLT_TIMESTAMP, C.SQLT_DAT:
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_TIMESTAMP,
				C.size_t(sizeOfNilPointer),
			); ret.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(ret.rv)
			} else {

				oci8cols[i].kind = C.SQLT_TIMESTAMP
				oci8cols[i].size = int(sizeOfNilPointer)
				oci8cols[i].pbuf = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
			}

		case C.SQLT_TIMESTAMP_TZ, C.SQLT_TIMESTAMP_LTZ:
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_TIMESTAMP_TZ,
				C.size_t(sizeOfNilPointer),
			); ret.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(ret.rv)
			} else {

				oci8cols[i].kind = C.SQLT_TIMESTAMP_TZ
				oci8cols[i].size = int(sizeOfNilPointer)
				oci8cols[i].pbuf = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
			}

		case C.SQLT_INTERVAL_DS:
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_INTERVAL_DS,
				C.size_t(sizeOfNilPointer),
			); ret.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(ret.rv)
			} else {

				oci8cols[i].kind = C.SQLT_INTERVAL_DS
				oci8cols[i].size = int(sizeOfNilPointer)
				oci8cols[i].pbuf = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
			}

		case C.SQLT_INTERVAL_YM:
			if ret := C.WrapOCIDescriptorAlloc(
				unsafe.Pointer(stmt.conn.env),
				C.OCI_DTYPE_INTERVAL_YM,
				C.size_t(sizeOfNilPointer),
			); ret.rv != C.OCI_SUCCESS {
				C.free(indrlenptr)
				return nil, stmt.conn.getError(ret.rv)
			} else {

				oci8cols[i].kind = C.SQLT_INTERVAL_YM
				oci8cols[i].size = int(sizeOfNilPointer)
				oci8cols[i].pbuf = ret.extra
				*(*unsafe.Pointer)(ret.extra) = ret.ptr
			}

		case C.SQLT_RDD: // rowid
			dataSize = 40
			oci8cols[i].kind = C.SQLT_CHR
			oci8cols[i].size = int(dataSize + 1)
			oci8cols[i].pbuf = C.malloc(C.size_t(dataSize) + 1)

		default:
			oci8cols[i].kind = C.SQLT_CHR
			oci8cols[i].size = int(dataSize + 1)
			oci8cols[i].pbuf = C.malloc(C.size_t(dataSize) + 1)
		}

		oci8cols[i].ind = &indrlen[i].ind
		oci8cols[i].rlen = &indrlen[i].rlen

		if rv := C.OCIDefineByPos(
			stmt.stmt,
			stmt.defp,
			stmt.conn.errHandle,
			C.ub4(i+1),
			oci8cols[i].pbuf,
			C.sb4(oci8cols[i].size),
			oci8cols[i].kind,
			unsafe.Pointer(oci8cols[i].ind),
			oci8cols[i].rlen,
			nil,
			C.OCI_DEFAULT,
		); rv != C.OCI_SUCCESS {
			C.free(indrlenptr)
			return nil, stmt.conn.getError(rv)
		}
	}

	rows := &OCI8Rows{
		stmt:       stmt,
		cols:       oci8cols,
		e:          false,
		indrlenptr: indrlenptr,
		closed:     false,
		done:       make(chan struct{}),
		cls:        closeRows,
	}

	go func() {
		select {
		case <-rows.done:
		case <-ctx.Done():
			// select again to avoid race condition if both are done
			select {
			case <-rows.done:
			default:
				C.OCIBreak(unsafe.Pointer(stmt.conn.svc), stmt.conn.errHandle)
				rows.Close()
			}
		}
	}()

	return rows, nil
}

// lastInsertId returns the last inserted ID
func (stmt *OCI8Stmt) lastInsertId() (int64, error) {
	// OCI_ATTR_ROWID must be get in handle -> alloc
	// can be coverted to char, but not to int64
	retRowid := C.WrapOCIAttrRowId(unsafe.Pointer(stmt.conn.env), unsafe.Pointer(stmt.stmt), C.OCI_HTYPE_STMT, C.OCI_ATTR_ROWID, stmt.conn.errHandle)
	if retRowid.rv == C.OCI_SUCCESS {
		bs := make([]byte, 18)
		for i, b := range retRowid.rowid[:18] {
			bs[i] = byte(b)
		}
		rowid := string(bs)
		return int64(uintptr(unsafe.Pointer(&rowid))), nil
	}
	return int64(0), nil
}

// rowsAffected returns the number of rows affected
func (stmt *OCI8Stmt) rowsAffected() (int64, error) {
	retUb4 := C.WrapOCIAttrGetUb4(unsafe.Pointer(stmt.stmt), C.OCI_HTYPE_STMT, C.OCI_ATTR_ROW_COUNT, stmt.conn.errHandle)
	if retUb4.rv != C.OCI_SUCCESS {
		return 0, stmt.conn.getError(retUb4.rv)
	}
	return int64(retUb4.num), nil
}

// Exec runs an exec query
func (stmt *OCI8Stmt) Exec(args []driver.Value) (r driver.Result, err error) {
	list := make([]namedValue, len(args))
	for i, v := range args {
		list[i] = namedValue{
			Ordinal: i + 1,
			Value:   v,
		}
	}
	return stmt.exec(context.Background(), list)
}

// exec runs an exec query
func (stmt *OCI8Stmt) exec(ctx context.Context, args []namedValue) (driver.Result, error) {
	var err error
	var fbp []oci8bind

	if fbp, err = stmt.bind(args); err != nil {
		return nil, err
	}

	defer freeBoundParameters(fbp)

	mode := C.ub4(C.OCI_DEFAULT)
	if stmt.conn.inTransaction == false {
		mode = mode | C.OCI_COMMIT_ON_SUCCESS
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			// select again to avoid race condition if both are done
			select {
			case <-done:
			default:
				C.OCIBreak(
					unsafe.Pointer(stmt.conn.svc),
					stmt.conn.errHandle)
			}
		}
	}()

	rv := C.OCIStmtExecute(
		stmt.conn.svc,
		stmt.stmt,
		stmt.conn.errHandle,
		1,
		0,
		nil,
		nil,
		mode)
	close(done)
	if rv != C.OCI_SUCCESS && rv != C.OCI_SUCCESS_WITH_INFO {
		return nil, stmt.conn.getError(rv)
	}

	n, en := stmt.rowsAffected()
	var id int64
	var ei error
	if n > 0 {
		id, ei = stmt.lastInsertId()
	}

	err = stmt.outputBoundParameters(fbp)
	if err != nil {
		return nil, err
	}

	return &OCI8Result{stmt: stmt, n: n, errn: en, id: id, errid: ei}, nil
}

// outputBoundParameters sets bound parameters
func (stmt *OCI8Stmt) outputBoundParameters(boundParameters []oci8bind) error {
	var err error

	for i, col := range boundParameters {
		if col.pbuf != nil {
			switch v := col.out.(type) {
			case *string:
				if col.indicator == -1 { // string is null
					*v = "" // best attempt at Go nil string
				} else {
					*v = C.GoString((*C.char)(col.pbuf))
				}

			case *int:
				*v = int(getInt64(col.pbuf))
			case *int64:
				*v = getInt64(col.pbuf)
			case *int32:
				*v = int32(getInt64(col.pbuf))
			case *int16:
				*v = int16(getInt64(col.pbuf))
			case *int8:
				*v = int8(getInt64(col.pbuf))

			case *uint:
				*v = uint(getUint64(col.pbuf))
			case *uint64:
				*v = getUint64(col.pbuf)
			case *uint32:
				*v = uint32(getUint64(col.pbuf))
			case *uint16:
				*v = uint16(getUint64(col.pbuf))
			case *uint8:
				*v = uint8(getUint64(col.pbuf))
			case *uintptr:
				*v = uintptr(getUint64(col.pbuf))

			case *float64:
				buf := (*[8]byte)(col.pbuf)[0:8]
				var data float64
				err = binary.Read(bytes.NewReader(buf), binary.LittleEndian, &data)
				if err != nil {
					return fmt.Errorf("binary read for column %v - error: %v", i, err)
				}
				*v = data
			case *float32:
				// statment is using SQLT_BDOUBLE to bind
				// need to read as float64 because of the 8 bits
				buf := (*[8]byte)(col.pbuf)[0:8]
				var data float64
				err = binary.Read(bytes.NewReader(buf), binary.LittleEndian, &data)
				if err != nil {
					return fmt.Errorf("binary read for column %v - error: %v", i, err)
				}
				*v = float32(data)

			case *bool:
				buf := (*[1 << 30]byte)(col.pbuf)[0:1]
				*v = buf[0] != 0
			}
		}
	}

	return nil
}

// ociParamGet calls OCIParamGet then returns OCIParam and error.
// OCIDescriptorFree must be called on returned OCIParam.
func (stmt *OCI8Stmt) ociParamGet(position C.ub4) (*C.OCIParam, error) {
	paramTemp := &C.OCIParam{}
	param := &paramTemp

	result := C.OCIParamGet(
		unsafe.Pointer(stmt.stmt),                // A statement handle or describe handle
		C.OCI_HTYPE_STMT,                         // Handle type: OCI_HTYPE_STMT, for a statement handle
		stmt.conn.errHandle,                      // An error handle
		(*unsafe.Pointer)(unsafe.Pointer(param)), // A descriptor of the parameter at the position
		position, // Position number in the statement handle or describe handle. A parameter descriptor will be returned for this position.
	)

	return *param, stmt.conn.getError(result)
}

// ociAttrGet calls OCIAttrGet with OCIStmt then returns attribute size and error.
// The attribute value is stored into passed value.
func (stmt *OCI8Stmt) ociAttrGet(value unsafe.Pointer, attributeType C.ub4) (C.ub4, error) {
	var size C.ub4

	result := C.OCIAttrGet(
		unsafe.Pointer(stmt.stmt), // Pointer to a handle type
		C.OCI_HTYPE_STMT,          // The handle type: OCI_HTYPE_STMT, for a statement handle
		value,                     // Pointer to the storage for an attribute value
		&size,                     // The size of the attribute value
		attributeType,             // The attribute type: https://docs.oracle.com/cd/B19306_01/appdev.102/b14250/ociaahan.htm
		stmt.conn.errHandle,       // An error handle
	)

	return size, stmt.conn.getError(result)
}

// ociBindByName calls OCIBindByName.
// bindHandle has to be nil or a valid bind handle. If nil, will be allocated.
func (stmt *OCI8Stmt) ociBindByName(
	bindHandle **C.OCIBind,
	name []byte,
	value unsafe.Pointer,
	maxSize C.sb4,
	dataType C.ub2,
	indicator C.sb2,
) error {
	if bindHandle == nil {
		bindHandleTemp := &C.OCIBind{}
		bindHandle = &bindHandleTemp
	}
	result := C.OCIBindByName(
		stmt.stmt,                  // The statement handle
		bindHandle,                 // The bind handle that is implicitly allocated by this call. The handle is freed implicitly when the statement handle is deallocated.
		stmt.conn.errHandle,        // An error handle
		(*C.OraText)(&name[0]),     // The placeholder, specified by its name, that maps to a variable in the statement associated with the statement handle.
		C.sb4(len(name)),           // The length of the name specified in placeholder, in number of bytes regardless of the encoding.
		value,                      // The pointer to a data value or an array of data values of type specified in the dty parameter
		maxSize,                    // The maximum size possible in bytes of any data value for this bind variable
		dataType,                   // The data type of the values being bound
		unsafe.Pointer(&indicator), // Pointer to an indicator variable or array
		nil,           // Pointer to the array of actual lengths of array elements
		nil,           // Pointer to the array of column-level return codes
		0,             // A maximum array length parameter
		nil,           // Current array length parameter
		C.OCI_DEFAULT, // The mode. Recommended to set to OCI_DEFAULT, which makes the bind variable have the same encoding as its statement.
	)

	return stmt.conn.getError(result)
}

// ociBindByPos calls OCIBindByPos.
// bindHandle has to be nil or a valid bind handle. If nil, will be allocated.
func (stmt *OCI8Stmt) ociBindByPos(
	bindHandle **C.OCIBind,
	position C.ub4,
	value unsafe.Pointer,
	maxSize C.sb4,
	dataType C.ub2,
	indicator C.sb2,
) error {
	if bindHandle == nil {
		bindHandleTemp := &C.OCIBind{}
		bindHandle = &bindHandleTemp
	}
	result := C.OCIBindByPos(
		stmt.stmt,                  // The statement handle
		bindHandle,                 // The bind handle that is implicitly allocated by this call. The handle is freed implicitly when the statement handle is deallocated.
		stmt.conn.errHandle,        // An error handle
		position,                   // The placeholder attributes are specified by position if OCIBindByPos() is being called.
		value,                      // An address of a data value or an array of data values
		maxSize,                    // The maximum size possible in bytes of any data value for this bind variable
		dataType,                   // The data type of the values being bound
		unsafe.Pointer(&indicator), // Pointer to an indicator variable or array
		nil,           // Pointer to the array of actual lengths of array elements
		nil,           // Pointer to the array of column-level return codes
		0,             // A maximum array length parameter
		nil,           // Current array length parameter
		C.OCI_DEFAULT, // The mode. Recommended to set to OCI_DEFAULT, which makes the bind variable have the same encoding as its statement.
	)

	return stmt.conn.getError(result)
}
