// Code that handles the control loop, and en/decoding messages
// to/from the kernel.  Dispatches calls into RawFileSystem.

package fuse

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// TODO make generic option setting.
const (
	// bufSize should be a power of two to minimize lossage in
	// BufferPool.
	bufSize = (1 << 16)
	maxRead = bufSize - PAGESIZE
)

type request struct {
	inputBuf []byte

	// These split up inputBuf.
	inHeader *InHeader      // generic header
	inData   unsafe.Pointer // per op data
	arg      []byte         // flat data.

	// Unstructured data, a pointer to the relevant XxxxOut struct.
	outData  unsafe.Pointer
	status   Status
	flatData []byte

	// Header + structured data for what we send back to the kernel.
	// May be followed by flatData.
	outHeaderBytes []byte

	// Start timestamp for timing info.
	startNs    int64
	preWriteNs int64
}

func (me *request) filename() string {
	return strings.TrimRight(string(me.arg), "\x00")
}

func (me *request) filenames(count int) []string {
	return strings.Split(string(me.arg), "\x00", count)
}


////////////////////////////////////////////////////////////////
// State related to this mount point.
type MountState struct {
	// Empty if unmounted.
	mountPoint string
	fileSystem RawFileSystem

	// I/O with kernel and daemon.
	mountFile *os.File

	// Dump debug info onto stdout.
	Debug bool

	// For efficient reads and writes.
	buffers *BufferPool

	*LatencyMap
}

// Mount filesystem on mountPoint.
func (me *MountState) Mount(mountPoint string) os.Error {
	file, mp, err := mount(mountPoint)
	if err != nil {
		return err
	}
	me.mountPoint = mp
	me.mountFile = file
	return nil
}

func (me *MountState) SetRecordStatistics(record bool) {
	if record {
		me.LatencyMap = NewLatencyMap()
	} else {
		me.LatencyMap = nil
	}
}

func (me *MountState) Unmount() os.Error {
	// Todo: flush/release all files/dirs?
	result := unmount(me.mountPoint)
	if result == nil {
		me.mountPoint = ""
	}
	return result
}

func (me *MountState) Write(req *request) {
	if me.LatencyMap != nil {
		req.preWriteNs = time.Nanoseconds()
	}

	if req.outHeaderBytes == nil {
		return
	}

	var err os.Error
	if req.flatData == nil {
		_, err = me.mountFile.Write(req.outHeaderBytes)
	} else {
		_, err = Writev(me.mountFile.Fd(),
			[][]byte{req.outHeaderBytes, req.flatData})
	}

	if err != nil {
		log.Printf("writer: Write/Writev %v failed, err: %v. Opcode: %v",
			req.outHeaderBytes, err, operationName(req.inHeader.Opcode))
	}
}

func NewMountState(fs RawFileSystem) *MountState {
	me := new(MountState)
	me.mountPoint = ""
	me.fileSystem = fs
	me.buffers = NewBufferPool()
	return me
}

func (me *MountState) Latencies() map[string]float64 {
	return me.LatencyMap.Latencies(1e-3)
}

func (me *MountState) OperationCounts() map[string]int {
	return me.LatencyMap.Counts()
}

func (me *MountState) BufferPoolStats() string {
	return me.buffers.String()
}

////////////////////////////////////////////////////////////////
// Logic for the control loop.

func (me *MountState) newRequest(oldReq *request) *request {
	if oldReq != nil {
		me.buffers.FreeBuffer(oldReq.flatData)

		*oldReq = request{
		status: OK,
		inputBuf: oldReq.inputBuf[0:bufSize],
		}
		return oldReq
	} 
		
	return &request{
		status: OK,
		inputBuf: me.buffers.AllocBuffer(bufSize),
	}
}

func (me *MountState) readRequest(req *request) os.Error {
	n, err := me.mountFile.Read(req.inputBuf)
	// If we start timing before the read, we may take into
	// account waiting for input into the timing.
	if me.LatencyMap != nil {
		req.startNs = time.Nanoseconds()
	}
	req.inputBuf = req.inputBuf[0:n]
	return err
}

func (me *MountState) discardRequest(req *request) {
	if me.LatencyMap != nil {
		endNs := time.Nanoseconds()
		dt := endNs - req.startNs

		opname := operationName(req.inHeader.Opcode)
		me.LatencyMap.AddMany(
			[]LatencyArg{
				{opname, "", dt},
				{opname + "-write", "", endNs - req.preWriteNs}})
	}
}

// Normally, callers should run Loop() and wait for FUSE to exit, but
// tests will want to run this in a goroutine.
//
// If threaded is given, each filesystem operation executes in a
// separate goroutine.
func (me *MountState) Loop(threaded bool) {
	// To limit scheduling overhead, we spawn multiple read loops.
	// This means that the request once read does not need to be
	// assigned to another thread, so it avoids a context switch.
	if threaded {
		for i := 0; i < _BACKGROUND_TASKS; i++ {
			go me.loop()
		}
	}
	me.loop()
	me.mountFile.Close()
}

func (me *MountState) loop() {
	// See fuse_kern_chan_receive()
	var lastReq *request
	for {
		req := me.newRequest(lastReq)
		lastReq = req
		err := me.readRequest(req)
		if err != nil {
			errNo := OsErrorToErrno(err)
 
			// Retry.
			if errNo == syscall.ENOENT {
				me.discardRequest(req)
				continue
			}

			// According to fuse_chan_receive()
			if errNo == syscall.ENODEV {
				break
			}

			// What I see on linux-x86 2.6.35.10.
			if errNo == syscall.ENOSYS {
				break
			}

			log.Printf("Failed to read from fuse conn: %v", err)
			break
		}
		me.handle(req)
	}
}


func (me *MountState) chopMessage(req *request) *operationHandler {
	inHSize := unsafe.Sizeof(InHeader{})
	if len(req.inputBuf) < inHSize {
		log.Printf("Short read for input header: %v", req.inputBuf)
		return nil
	}

	req.inHeader = (*InHeader)(unsafe.Pointer(&req.inputBuf[0]))
	req.arg = req.inputBuf[inHSize:]

	handler := getHandler(req.inHeader.Opcode)
	if handler == nil || handler.Func == nil {
		log.Printf("Unknown opcode %v", req.inHeader.Opcode)
		req.status = ENOSYS
		return handler
	}

	if len(req.arg) < handler.InputSize {
		log.Printf("Short read for %v: %v", req.inHeader.Opcode, req.arg)
		req.status = EIO
		return handler
	}

	if handler.InputSize > 0 {
		req.inData = unsafe.Pointer(&req.arg[0])
		req.arg = req.arg[handler.InputSize:]
	}
	return handler
}

func (me *MountState) handle(req *request) {
	defer me.discardRequest(req)
	handler := me.chopMessage(req)

	if handler == nil {
		return
	}

	if req.status == OK {
		me.dispatch(req, handler)
	}

	// If we try to write OK, nil, we will get
	// error:  writer: Writev [[16 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0]]
	// failed, err: writev: no such file or directory
	if req.inHeader.Opcode != FUSE_FORGET {
		serialize(req, handler, me.Debug)
		me.Write(req)
	}
}

func (me *MountState) dispatch(req *request, handler *operationHandler) {
	if me.Debug {
		nm := ""
		// TODO - reinstate filename printing.
		log.Printf("Dispatch: %v, NodeId: %v %s\n",
			operationName(req.inHeader.Opcode), req.inHeader.NodeId, nm)
	}
	handler.Func(me, req)
}

// Thanks to Andrew Gerrand for this hack.
func asSlice(ptr unsafe.Pointer, byteCount int) []byte {
	h := &reflect.SliceHeader{uintptr(ptr), byteCount, byteCount}
	return *(*[]byte)(unsafe.Pointer(h))
}

func serialize(req *request, handler *operationHandler, debug bool) {
	dataLength := handler.OutputSize
	if req.outData == nil || req.status != OK {
		dataLength = 0
	}

	sizeOfOutHeader := unsafe.Sizeof(OutHeader{})

	req.outHeaderBytes = make([]byte, sizeOfOutHeader+dataLength)
	outHeader := (*OutHeader)(unsafe.Pointer(&req.outHeaderBytes[0]))
	outHeader.Unique = req.inHeader.Unique
	outHeader.Status = -req.status
	outHeader.Length = uint32(sizeOfOutHeader + dataLength + len(req.flatData))

	copy(req.outHeaderBytes[sizeOfOutHeader:], asSlice(req.outData, dataLength))
	if debug {
		val := fmt.Sprintf("%v", replyString(req.inHeader.Opcode, req.outData))
		max := 1024
		if len(val) > max {
			val = val[:max] + fmt.Sprintf(" ...trimmed (response size %d)", outHeader.Length)
		}

		msg := ""
		if len(req.flatData) > 0 {
			msg = fmt.Sprintf(" flat: %d\n", len(req.flatData))
		}
		log.Printf("Serialize: %v code: %v value: %v%v",
			operationName(req.inHeader.Opcode), req.status, val, msg)
	}
}
