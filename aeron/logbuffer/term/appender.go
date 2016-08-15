/*
Copyright 2016 Stanislav Liberman

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package term

import (
	"github.com/lirm/aeron-go/aeron/atomic"
	"github.com/lirm/aeron-go/aeron/flyweight"
	"github.com/lirm/aeron-go/aeron/logbuffer"
	"github.com/lirm/aeron-go/aeron/util"
	"math"
	"unsafe"
)

const (
	// AppenderTripped is returned when the end of the term has been reached and buffer roll was done
	AppenderTripped int64 = -1

	// AppenderFailed is returned when appending is not possible due to position being outside of the term. ??
	AppenderFailed int64 = -2

	beginFrag    uint8 = 0x80
	endFrag      uint8 = 0x40
	unfragmented uint8 = 0x80 | 0x40
)

// DefaultReservedValueSupplier is the default reserved value provider
var DefaultReservedValueSupplier ReservedValueSupplier = func(termBuffer *atomic.Buffer, termOffset int32, length int32) int64 { return 0 }

// ReservedValueSupplier is the type definition for a provider of user supplied header data
type ReservedValueSupplier func(termBuffer *atomic.Buffer, termOffset int32, length int32) int64

// HeaderWriter is a helper class for writing frame header to the term
type headerWriter struct {
	sessionID int32
	streamID  int32
	buffer    atomic.Buffer
}

func (header *headerWriter) fill(defaultHdr *atomic.Buffer) {
	header.sessionID = defaultHdr.GetInt32(logbuffer.DataFrameHeader.SessionIDFieldOffset)
	header.streamID = defaultHdr.GetInt32(logbuffer.DataFrameHeader.StreamIDFieldOffset)
}

func (header *headerWriter) write(termBuffer *atomic.Buffer, offset, length, termID int32) {
	termBuffer.PutInt32Ordered(offset, -length)

	headerPtr := uintptr(termBuffer.Ptr()) + uintptr(offset)
	header.buffer.Wrap(unsafe.Pointer(headerPtr), logbuffer.DataFrameHeader.Length)

	header.buffer.PutInt8(logbuffer.DataFrameHeader.VersionFieldOffset, logbuffer.DataFrameHeader.CurrentVersion)
	header.buffer.PutUInt8(logbuffer.DataFrameHeader.FlagsFieldOffset, unfragmented)
	header.buffer.PutUInt16(logbuffer.DataFrameHeader.TypeFieldOffset, logbuffer.DataFrameHeader.TypeData)
	header.buffer.PutInt32(logbuffer.DataFrameHeader.TermOffsetFieldOffset, offset)
	header.buffer.PutInt32(logbuffer.DataFrameHeader.SessionIDFieldOffset, header.sessionID)
	header.buffer.PutInt32(logbuffer.DataFrameHeader.StreamIDFieldOffset, header.streamID)
	header.buffer.PutInt32(logbuffer.DataFrameHeader.TermIDFieldOffset, termID)
}

// Appender type is the term writer
type Appender struct {
	termBuffer   *atomic.Buffer
	tailCounter  flyweight.Int64Field
	headerWriter headerWriter
}

// AppenderResult is a helper structure for a zero-copy tuple return. Can likely be done with Go's tuple return
type AppenderResult struct {
	termOffset int64
	termID     int32
}

// TermOffset returns current term offset
func (result *AppenderResult) TermOffset() int64 {
	return result.termOffset
}

// TermID return current term ID
func (result *AppenderResult) TermID() int32 {
	return result.termID
}

// MakeAppender is the factory function for term Appenders
func MakeAppender(logBuffers *logbuffer.LogBuffers, partitionIndex int) *Appender {

	appender := new(Appender)
	appender.termBuffer = logBuffers.Buffer(partitionIndex)
	appender.tailCounter = logBuffers.Meta().TailCounter[partitionIndex]

	header := logBuffers.Meta().DefaultFrameHeader.Get()
	appender.headerWriter.fill(header)

	return appender
}

// RawTail is the accessor to the raw value of the tail offset used by Publication
func (appender *Appender) RawTail() int64 {
	return appender.tailCounter.Get()
}

func (appender *Appender) getAndAddRawTail(alignedLength int32) int64 {
	return appender.tailCounter.GetAndAddInt64(int64(alignedLength))
}

// Claim is the interface for using Buffer Claims for zero copy sends
func (appender *Appender) Claim(result *AppenderResult, length int32, claim *logbuffer.Claim) {

	frameLength := length + logbuffer.DataFrameHeader.Length
	alignedLength := util.AlignInt32(frameLength, logbuffer.FrameAlignment)
	rawTail := appender.getAndAddRawTail(alignedLength)
	termOffset := rawTail & 0xFFFFFFFF

	termLength := appender.termBuffer.Capacity()

	result.termID = logbuffer.TermID(rawTail)
	result.termOffset = termOffset + int64(alignedLength)
	if result.termOffset > int64(termLength) {
		result.termOffset = handleEndOfLogCondition(result.termID, appender.termBuffer, int32(termOffset),
			&appender.headerWriter, termLength)
	} else {
		offset := int32(termOffset)
		appender.headerWriter.write(appender.termBuffer, offset, frameLength, result.termID)
		claim.Wrap(appender.termBuffer, offset, frameLength)
	}
}

// AppendUnfragmentedMessage appends an unfragmented message in a single frame to the term
func (appender *Appender) AppendUnfragmentedMessage(result *AppenderResult,
	srcBuffer *atomic.Buffer, srcOffset int32, length int32, reservedValueSupplier ReservedValueSupplier) {

	frameLength := length + logbuffer.DataFrameHeader.Length
	alignedLength := util.AlignInt32(frameLength, logbuffer.FrameAlignment)
	rawTail := appender.getAndAddRawTail(alignedLength)
	termOffset := rawTail & 0xFFFFFFFF

	termLength := appender.termBuffer.Capacity()

	result.termID = logbuffer.TermID(rawTail)
	result.termOffset = termOffset + int64(alignedLength)
	if result.termOffset > int64(termLength) {
		result.termOffset = handleEndOfLogCondition(result.termID, appender.termBuffer, int32(termOffset),
			&appender.headerWriter, termLength)
	} else {
		offset := int32(termOffset)
		appender.headerWriter.write(appender.termBuffer, offset, frameLength, logbuffer.TermID(rawTail))
		appender.termBuffer.PutBytes(offset+logbuffer.DataFrameHeader.Length, srcBuffer, srcOffset, length)

		if nil != reservedValueSupplier {
			reservedValue := reservedValueSupplier(appender.termBuffer, offset, frameLength)
			appender.termBuffer.PutInt64(offset+logbuffer.DataFrameHeader.ReservedValueFieldOffset, reservedValue)
		}

		logbuffer.FrameLengthOrdered(appender.termBuffer, offset, frameLength)
	}
}

// AppendFragmentedMessage appends a message greater than frame length as a batch of fragments
func (appender *Appender) AppendFragmentedMessage(result *AppenderResult,
	srcBuffer *atomic.Buffer, srcOffset int32, length int32, maxPayloadLength int32, reservedValueSupplier ReservedValueSupplier) {

	numMaxPayloads := length / maxPayloadLength
	remainingPayload := length % maxPayloadLength
	var lastFrameLength int32
	if remainingPayload > 0 {
		lastFrameLength = util.AlignInt32(remainingPayload+logbuffer.DataFrameHeader.Length, logbuffer.FrameAlignment)
	}
	requiredLength := (numMaxPayloads * (maxPayloadLength + logbuffer.DataFrameHeader.Length)) + lastFrameLength
	rawTail := appender.getAndAddRawTail(requiredLength)
	termOffset := rawTail & 0xFFFFFFFF

	termLength := appender.termBuffer.Capacity()

	result.termID = logbuffer.TermID(rawTail)
	result.termOffset = termOffset + int64(requiredLength)
	if result.termOffset > int64(termLength) {
		result.termOffset = handleEndOfLogCondition(result.termID, appender.termBuffer, int32(termOffset),
			&appender.headerWriter, termLength)
	} else {
		flags := beginFrag
		remaining := length
		offset := int32(termOffset)

		for remaining > 0 {
			bytesToWrite := int32(math.Min(float64(remaining), float64(maxPayloadLength)))
			frameLength := bytesToWrite + logbuffer.DataFrameHeader.Length
			alignedLength := util.AlignInt32(frameLength, logbuffer.FrameAlignment)

			appender.headerWriter.write(appender.termBuffer, offset, frameLength, result.termID)
			appender.termBuffer.PutBytes(
				offset+logbuffer.DataFrameHeader.Length, srcBuffer, srcOffset+(length-remaining), bytesToWrite)

			if remaining <= maxPayloadLength {
				flags |= endFrag
			}

			logbuffer.FrameFlags(appender.termBuffer, offset, flags)

			reservedValue := reservedValueSupplier(appender.termBuffer, offset, frameLength)
			appender.termBuffer.PutInt64(offset+logbuffer.DataFrameHeader.ReservedValueFieldOffset, reservedValue)

			logbuffer.FrameLengthOrdered(appender.termBuffer, offset, frameLength)

			flags = 0
			offset += alignedLength
			remaining -= bytesToWrite
		}
	}
}

func handleEndOfLogCondition(termID int32, termBuffer *atomic.Buffer, termOffset int32,
	header *headerWriter, termLength int32) int64 {
	newOffset := AppenderFailed

	if termOffset <= termLength {
		newOffset = AppenderTripped

		if termOffset < termLength {
			paddingLength := termLength - termOffset
			header.write(termBuffer, termOffset, paddingLength, termID)
			logbuffer.SetFrameType(termBuffer, termOffset, logbuffer.DataFrameHeader.TypePad)
			logbuffer.FrameLengthOrdered(termBuffer, termOffset, paddingLength)
		}
	}

	return newOffset
}

func (appender *Appender) SetTailTermID(termID int32) {
	appender.tailCounter.Set(int64(termID) << 32)
}
