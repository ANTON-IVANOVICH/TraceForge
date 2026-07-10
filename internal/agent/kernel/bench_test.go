package kernel

import (
	"strconv"
	"strings"
	"testing"

	"metrics-system/internal/model"
	"metrics-system/internal/testutil"
)

// A full /proc/net/snmp as a modern kernel prints it: every section, in order,
// with the real column names. This is what the parser walks past on the way to
// the four Tcp/Udp fields the collector keeps.
const fullSNMP = `Ip: Forwarding DefaultTTL InReceives InHdrErrors InAddrErrors ForwDatagrams InUnknownProtos InDiscards InDelivers OutRequests OutDiscards OutNoRoutes ReasmTimeout ReasmReqds ReasmOKs ReasmFails FragOKs FragFails FragCreates
Ip: 1 64 1234567 0 0 0 0 0 1234000 1200000 12 34 0 0 0 0 0 0 0
Icmp: InMsgs InErrors InCsumErrors InDestUnreachs InTimeExcds InParmProbs InSrcQuenchs InRedirects InEchos InEchoReps InTimestamps InTimestampReps InAddrMasks InAddrMaskReps OutMsgs OutErrors OutRateLimitGlobal OutRateLimitHost OutDestUnreachs OutTimeExcds OutParmProbs OutSrcQuenchs OutRedirects OutEchos OutEchoReps OutTimestamps OutTimestampReps OutAddrMasks OutAddrMaskReps
Icmp: 100 2 0 50 0 0 0 0 40 8 0 0 0 0 110 0 0 0 60 0 0 0 0 8 40 0 0 0 0
IcmpMsg: InType3 InType8 OutType0 OutType3 OutType8
IcmpMsg: 50 40 8 60 40
Tcp: RtoAlgorithm RtoMin RtoMax MaxConn ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts InCsumErrors
Tcp: 1 200 120000 -1 54321 12345 100 50 42 9876543 8765432 1234 0 567 0
Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
Udp: 456789 321 12 445566 0 0 0 5 0
UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
UdpLite: 0 0 0 0 0 0 0 0 0
`

// The TcpExt column list as it appears on a recent kernel — ~130 fields, which
// is where /proc/net/netstat gets its length. ListenOverflows and ListenDrops
// sit deep in the middle, exactly where the collector must find them by name.
const tcpExtColumns = `SyncookiesSent SyncookiesRecv SyncookiesFailed EmbryonicRsts PruneCalled RcvPruned OfoPruned OutOfWindowIcmps LockDroppedIcmps ArpFilter TW TWRecycled TWKilled PAWSActive PAWSEstab DelayedACKs DelayedACKLocked DelayedACKLost ListenOverflows ListenDrops TCPHPHits TCPPureAcks TCPHPAcks TCPRenoRecovery TCPSackRecovery TCPSACKReneging TCPSACKReorder TCPRenoReorder TCPTSReorder TCPFullUndo TCPPartialUndo TCPDSACKUndo TCPLossUndo TCPLostRetransmit TCPRenoFailures TCPSackFailures TCPLossFailures TCPFastRetrans TCPSlowStartRetrans TCPTimeouts TCPLossProbes TCPLossProbeRecovery TCPRenoRecoveryFail TCPSackRecoveryFail TCPRcvCollapsed TCPBacklogCoalesce TCPDSACKOldSent TCPDSACKOfoSent TCPDSACKRecv TCPDSACKOfoRecv TCPAbortOnData TCPAbortOnClose TCPAbortOnMemory TCPAbortOnTimeout TCPAbortOnLinger TCPAbortFailed TCPMemoryPressures TCPMemoryPressuresChrono TCPSACKDiscard TCPDSACKIgnoredOld TCPDSACKIgnoredNoUndo TCPSpuriousRTOs TCPMD5NotFound TCPMD5Unexpected TCPMD5Failure TCPSackShifted TCPSackMerged TCPSackShiftFallback TCPBacklogDrop PFMemallocDrop TCPMinTTLDrop TCPDeferAcceptDrop IPReversePathFilter TCPTimeWaitOverflow TCPReqQFullDoCookies TCPReqQFullDrop TCPRetransFail TCPRcvCoalesce TCPOFOQueue TCPOFODrop TCPOFOMerge TCPChallengeACK TCPSYNChallenge TCPFastOpenActive TCPFastOpenActiveFail TCPFastOpenPassive TCPFastOpenPassiveFail TCPFastOpenListenOverflow TCPFastOpenCookieReqd TCPFastOpenBlackhole TCPSpuriousRtxHostQueues BusyPollRxPackets TCPAutoCorking TCPFromZeroWindowAdv TCPToZeroWindowAdv TCPWantZeroWindowAdv TCPSynRetrans TCPOrigDataSent TCPHystartTrainDetect TCPHystartTrainCwnd TCPHystartDelayDetect TCPHystartDelayCwnd TCPACKSkippedSynRecv TCPACKSkippedPAWS TCPACKSkippedSeq TCPACKSkippedFinWait2 TCPACKSkippedTimeWait TCPACKSkippedChallenge TCPWinProbe TCPKeepAlive TCPMTUPFail TCPMTUPSuccess TCPDelivered TCPDeliveredCE TCPAckCompressed TCPZeroWindowDrop TCPRcvQDrop TCPWqueueTooBig TCPFastOpenPassiveAltKey TcpTimeoutRehash TcpDuplicateDataRehash TCPDSACKRecvSegs TCPDSACKIgnoredDubious TCPMigrateReqSuccess TCPMigrateReqFailure`

const ipExtColumns = `InNoRoutes InTruncatedPkts InMcastPkts OutMcastPkts InBcastPkts OutBcastPkts InOctets OutOctets InMcastOctets OutMcastOctets InBcastOctets OutBcastOctets InCsumErrors InNoECTPkts InECT1Pkts InECT0Pkts InCEPkts ReasmOverlaps`

// fullNetstat renders a realistic /proc/net/netstat: the two long TcpExt/IpExt
// sections with value lines whose length matches their headers by construction,
// so the fixture can never drift into the mismatch path and quietly measure
// nothing.
func fullNetstat() string {
	var b strings.Builder
	section := func(prefix, columns string) {
		b.WriteString(prefix + ": " + columns + "\n")
		b.WriteString(prefix + ":")
		for i := range strings.Fields(columns) {
			b.WriteByte(' ')
			b.WriteString(strconv.Itoa(i * 7)) // arbitrary non-negative values
		}
		b.WriteByte('\n')
	}
	section("TcpExt", tcpExtColumns)
	section("IpExt", ipExtColumns)
	return b.String()
}

var benchMetricSink []model.Metric

// The headline number: parsing a full snmp + full netstat, the exact work
// Collect does on every tick minus the file open. It runs in tens of
// microseconds against a collection interval measured in seconds, so its cost is
// noise — which is the point. The pure-Go route to these counters is not just
// free of a C toolchain, it is fast enough that there was never a performance
// case for the eBPF/CGo alternative in the first place.
func BenchmarkParseProcNet(b *testing.B) {
	snmp := fullSNMP
	netstat := fullNetstat()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseProcNet(strings.NewReader(snmp)); err != nil {
			b.Fatal(err)
		}
		if _, err := parseProcNet(strings.NewReader(netstat)); err != nil {
			b.Fatal(err)
		}
	}
}

// The whole reader-level pipeline: parse both files, merge, and curate down to
// the emitted set. This is Collect without the two os.Open calls.
func BenchmarkCollect(b *testing.B) {
	snmp := fullSNMP
	netstat := fullNetstat()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, err := collect(strings.NewReader(snmp), strings.NewReader(netstat), "host", testutil.BaseTime)
		if err != nil {
			b.Fatal(err)
		}
		benchMetricSink = m
	}
}
