package psst

//go:generate stringer -type=connState
//go:generate stringer -type=action

import (
	"container/list"
	"fmt"
	"math/rand"
	"time"
)

// Connection states
type connState int

const (
	stateClosed connState = iota
	stateListen
	stateSynSent
	stateSynReceived
	stateOpen
	stateCloseWait
)

// Response action
type action int

const (
	actionContinue action = iota
	actionDiscard
	actionReset
	actionAck
)

type connConfig struct {
	MaxOutstandingSegmentsSelf uint16
	MaxOutstandingSegmentsPeer uint16
	RetransmissionTimeout      uint16
	CumulativeAckTimeout       uint16
	NulTimeout                 uint16
	MaxRetransmissions         uint8
	MaxCumulativeAck           uint8
	MaxOutOfSeq                uint8
}

type txBufferEntry struct {
	SeqNumber uint16
	txCount   uint8
	Data      []byte
}

type rxBufferEntry struct {
	SeqNumber uint16
	Data      []byte
}

type conn struct {
	state connState
	// Connection config
	config *connConfig
	// Transmitter state variables
	txNextSeq       uint16
	txOldestUnacked uint16
	txBuffer        *list.List
	// Receiver state variables
	rxLastInSeq uint16
	rxBuffer    *list.List
	// Timers
	retransmissionTimer *time.Timer
	cumulativeAckTimer  *time.Timer
	nulTimer            *time.Timer
}

func NewConn() *conn {
	initialSeqNumber := uint16(rand.Int())
	return &conn{
		state:           stateClosed,
		txNextSeq:       initialSeqNumber + 1,
		txOldestUnacked: initialSeqNumber,
		txBuffer:        list.New(),
		rxBuffer:        list.New(),
	}
}

func (self *conn) handleSegment(segment *segment) error {
	if action, err := self.validateSegment(segment); action != actionContinue {
		// TODO perform action
		return err
	}

	switch self.state {

	case stateSynSent:
		if segment.RST {
			self.closed()
			break
		}
		fallthrough

	case stateListen:
		synHeader := segment.VarHeader.(*synVarHeader)
		if err := self.handshakeConfig(synHeader); err != nil {
			// TODO Respond with RST
			return err
		}

		if segment.ACK {
			// TODO Respond with ACK
			self.connected()
		} else {
			// TODO Respond with SYN ACK
			self.state = stateSynReceived
		}

	case stateSynReceived:
		if segment.RST {
			self.closed()
			break
		}

		self.connected()
		fallthrough

	case stateOpen:
		// Handle RST & break
		if segment.RST {
			// TODO transition to stateCloseWait
			self.closed()
			break
		}

		// Handle NUL & break
		if segment.NUL {
			// TODO validate SeqNumber and respond with ACK
			break
		}

		// Handle ACK
		if segment.ACK {
			// Check for positive unsigned diff AckNumber > txOldestUnacked
			if diff := int16(segment.AckNumber - self.txOldestUnacked); diff > 0 {
				self.txOldestUnacked = segment.AckNumber
				self.clearAckedTxBuffer()
			}
		}

		// Handle EAK
		if segment.EAK {
			eakHeader := segment.VarHeader.(*eakVarHeader)
			for _, eak := range eakHeader.EakNumbers {
				self.removeFromTxBuffer(eak)
			}
		}

		// Handle data payload
		if len(segment.Data) > 0 {
			if segment.SeqNumber-self.rxLastInSeq == 1 {
				self.receivedData(segment.Data)
				self.rxLastInSeq++
				self.flushInSeqRxBuffer()
			} else {
				self.bufferRxData(segment.SeqNumber, segment.Data)
			}
		}

	case stateCloseWait:
		if segment.RST {
			self.closed()
			break
		}

	}

	return nil
}

func (self *conn) validateSegment(segment *segment) (action, error) {
	// Check for unexpected segment header
	switch self.state {

	case stateClosed:
		return actionDiscard, fmt.Errorf("Unexpected segment")

	case stateListen:
		if !segment.SYN || segment.ACK || segment.EAK || segment.RST || segment.NUL {
			return actionDiscard, fmt.Errorf("Invalid segment flags")
		}

		// Not sure if this is needed if already checked during deserialisation
		if _, ok := segment.VarHeader.(*synVarHeader); !ok {
			return actionReset, fmt.Errorf("SYN segment missing header")
		}

	case stateSynSent:
		if segment.RST {
			break
		}

		if !(segment.SYN && !segment.EAK && !segment.NUL) {
			return actionDiscard, fmt.Errorf("Invalid segment flags")
		}

		// Not sure if this is needed if already checked during deserialisation
		if _, ok := segment.VarHeader.(*synVarHeader); !ok {
			return actionReset, fmt.Errorf("SYN segment missing header")
		}

		if segment.ACK && segment.AckNumber != self.txNextSeq-1 {
			return actionReset, fmt.Errorf("Inital ACK does not match initial sequence number")
		}

	case stateSynReceived:
		if segment.RST {
			break
		}

		if segment.SYN || segment.EAK {
			return actionReset, fmt.Errorf("Invalid segment flags")
		}

		// Check sequence number is in valid range
		if diff := int16(segment.SeqNumber - self.rxLastInSeq); diff <= 0 || diff > int16(2*self.config.MaxOutstandingSegmentsSelf) {
			return actionAck, fmt.Errorf("Unexpected sequence number")
		}

		if segment.SYN || segment.EAK {
			return actionReset, fmt.Errorf("Invalid segment flags")
		}

		if !segment.ACK {
			return actionDiscard, fmt.Errorf("Need ACK for initial SYN before proceeding")
		}

		if segment.AckNumber != self.txNextSeq-1 {
			return actionReset, fmt.Errorf("Inital ACK does not match initial sequence number")
		}

	case stateOpen:
		if segment.RST {
			break
		}

		// Check sequence number is in valid range
		// Do this before checking other data to gracefully handle late or duplicate segments
		if diff := int16(segment.SeqNumber - self.rxLastInSeq); diff <= 0 || diff > int16(2*self.config.MaxOutstandingSegmentsSelf) {
			return actionAck, fmt.Errorf("Unexpected sequence number")
		}

		if segment.SYN {
			return actionReset, fmt.Errorf("Invalid segment flags")
		}

		if segment.NUL && len(segment.Data) > 0 {
			return actionDiscard, fmt.Errorf("NUL segment must not contain data payload")
		}

		if segment.ACK {
			if diff := int16(segment.AckNumber - self.txNextSeq); diff >= 0 {
				return actionDiscard, fmt.Errorf("ACK received for unsent sequence number")
			}
		}

		if segment.EAK {
			if !segment.ACK {
				return actionDiscard, fmt.Errorf("Invalid segment flags")
			}

			eakHeader, ok := segment.VarHeader.(*eakVarHeader)
			if !ok || len(eakHeader.EakNumbers) == 0 {
				return actionReset, fmt.Errorf("EAK segment missing header")
			}

			for _, eak := range eakHeader.EakNumbers {
				if diff := int16(eak - segment.AckNumber); diff < 0 {
					return actionDiscard, fmt.Errorf("EAK number smaller than segment ACK number")
				}
				if diff := int16(eak - self.txNextSeq); diff >= 0 {
					return actionDiscard, fmt.Errorf("EAK received for unsent sequence number")
				}
			}
		}

	case stateCloseWait:
		if !segment.RST {
			return actionDiscard, fmt.Errorf("Unexpected segment")
		}

	}

	return actionContinue, nil
}

func (self *conn) handshakeConfig(synHeader *synVarHeader) error {
	// TODO Validate config is compatible/agreeable
	// TODO Init connection config
	return nil
}

func (self *conn) removeFromTxBuffer(seqNumber uint16) {
	for element := self.txBuffer.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*txBufferEntry)

		if entry.SeqNumber == seqNumber {
			self.txBuffer.Remove(element)
			break
		}

		// Check for positive unsigned diff: buffer SeqNumber > input seqNumber
		if diff := int16(entry.SeqNumber - seqNumber); diff > 0 {
			break
		}
	}
}

func (self *conn) clearAckedTxBuffer() {
	var next *list.Element
	for element := self.txBuffer.Front(); element != nil; element = next {
		entry := element.Value.(*txBufferEntry)

		// Check for positive unsigned diff: SeqNumber > txOldestUnacked
		if diff := int16(entry.SeqNumber - self.txOldestUnacked); diff > 0 {
			break
		}

		next = element.Next()
		self.txBuffer.Remove(element)
	}
}

func (self *conn) receivedData(data []byte) {
	// TODO Forward new data to listener
}

func (self *conn) flushInSeqRxBuffer() {
	var next *list.Element
	for element := self.rxBuffer.Front(); element != nil; element = next {
		entry := element.Value.(*rxBufferEntry)

		if entry.SeqNumber-self.rxLastInSeq != 1 {
			break
		}

		next = element.Next()
		self.rxBuffer.Remove(element)
		self.receivedData(entry.Data)
		self.rxLastInSeq++
	}
}

func (self *conn) bufferRxData(seqNumber uint16, data []byte) {
	var element *list.Element
	for element = self.rxBuffer.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*rxBufferEntry)

		// Duplicate segment already buffered
		if entry.SeqNumber == seqNumber {
			return
		}

		// Check for positive unsigned diff: entry SeqNumber > input seqNumber
		if diff := int16(entry.SeqNumber - seqNumber); diff > 0 {
			break
		}
	}

	entry := &rxBufferEntry{
		SeqNumber: seqNumber,
		Data:      data,
	}

	if element != nil {
		self.rxBuffer.InsertBefore(entry, element)
	} else {
		self.rxBuffer.PushBack(entry)
	}
}

func (self *conn) connected() {
	// TODO Notify listeners, start timers
	self.state = stateOpen
}

func (self *conn) closed() {
	// TODO Clean up connection, timers, listeners etc.
	self.state = stateClosed
}
