import { Audio } from "expo-av";
import React, { useCallback, useEffect, useRef, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { Visit } from "../api";
import { WS_BASE_URL } from "../config";

interface Props {
  visit: Visit;
  onComplete: (visitId: number) => void;
  onBack: () => void;
}

type Phase = "recording" | "processing" | "done" | "error";

interface ServerMessage {
  type: "transcript_delta" | "transcript_final" | "note_delta" | "note_complete" | "error";
  text?: string;
  note?: object;
  message?: string;
}

const CHUNK_INTERVAL_MS = 3000; // send audio every 3 s

export default function RecordingScreen({ visit, onComplete, onBack }: Props) {
  const [phase, setPhase] = useState<Phase>("recording");
  const [transcriptSegments, setTranscriptSegments] = useState<string[]>([]);
  const [noteStreamBuffer, setNoteStreamBuffer] = useState("");
  const [elapsed, setElapsed] = useState(0);
  const [error, setError] = useState<string | null>(null);

  const wsRef = useRef<WebSocket | null>(null);
  const recordingRef = useRef<Audio.Recording | null>(null);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const chunkTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const connectWebSocket = useCallback(() => {
    const ws = new WebSocket(`${WS_BASE_URL}/ws/visits/${visit.id}/audio`);
    wsRef.current = ws;

    ws.onmessage = (event) => {
      const msg: ServerMessage = JSON.parse(event.data);
      if (msg.type === "transcript_delta" && msg.text) {
        setTranscriptSegments((prev) => [...prev, msg.text!]);
      } else if (msg.type === "note_delta" && msg.text) {
        setNoteStreamBuffer((prev) => prev + msg.text);
      } else if (msg.type === "note_complete") {
        setPhase("done");
        onComplete(visit.id);
      } else if (msg.type === "error") {
        setError(msg.message ?? "Unknown error");
        setPhase("error");
      }
    };

    ws.onerror = () => {
      setError("WebSocket connection failed — is the backend running?");
      setPhase("error");
    };
  }, [visit.id, onComplete]);

  const startRecording = useCallback(async () => {
    await Audio.requestPermissionsAsync();
    await Audio.setAudioModeAsync({ allowsRecordingIOS: true, playsInSilentModeIOS: true });

    const { recording } = await Audio.Recording.createAsync({
      android: {
        extension: ".wav",
        outputFormat: Audio.AndroidOutputFormat.DEFAULT,
        audioEncoder: Audio.AndroidAudioEncoder.DEFAULT,
        sampleRate: 16000,
        numberOfChannels: 1,
        bitRate: 256000,
      },
      ios: {
        extension: ".wav",
        outputFormat: Audio.IOSOutputFormat.LINEARPCM,
        audioQuality: Audio.IOSAudioQuality.HIGH,
        sampleRate: 16000,
        numberOfChannels: 1,
        bitRate: 256000,
        linearPCMBitDepth: 16,
        linearPCMIsBigEndian: false,
        linearPCMIsFloat: false,
      },
      web: {},
    });

    recordingRef.current = recording;
  }, []);

  const flushChunk = useCallback(async () => {
    const ws = wsRef.current;
    const recording = recordingRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN || !recording) return;

    // Stop current recording, read URI, restart
    await recording.stopAndUnloadAsync();
    const uri = recording.getURI();
    if (uri) {
      const response = await fetch(uri);
      const buffer = await response.arrayBuffer();
      ws.send(buffer);
    }

    // Start a fresh recording segment
    const { recording: newRec } = await Audio.Recording.createAsync({
      android: {
        extension: ".wav",
        outputFormat: Audio.AndroidOutputFormat.DEFAULT,
        audioEncoder: Audio.AndroidAudioEncoder.DEFAULT,
        sampleRate: 16000,
        numberOfChannels: 1,
        bitRate: 256000,
      },
      ios: {
        extension: ".wav",
        outputFormat: Audio.IOSOutputFormat.LINEARPCM,
        audioQuality: Audio.IOSAudioQuality.HIGH,
        sampleRate: 16000,
        numberOfChannels: 1,
        bitRate: 256000,
        linearPCMBitDepth: 16,
        linearPCMIsBigEndian: false,
        linearPCMIsFloat: false,
      },
      web: {},
    });
    recordingRef.current = newRec;
  }, []);

  useEffect(() => {
    connectWebSocket();
    startRecording();

    timerRef.current = setInterval(() => setElapsed((e) => e + 1), 1000);
    chunkTimerRef.current = setInterval(flushChunk, CHUNK_INTERVAL_MS);

    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
      if (chunkTimerRef.current) clearInterval(chunkTimerRef.current);
    };
  }, [connectWebSocket, startRecording, flushChunk]);

  const handleEndVisit = async () => {
    if (chunkTimerRef.current) clearInterval(chunkTimerRef.current);
    if (timerRef.current) clearInterval(timerRef.current);

    setPhase("processing");

    // Flush final chunk then signal END
    await flushChunk();
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send("END");
    }

    // Stop recording
    try {
      await recordingRef.current?.stopAndUnloadAsync();
    } catch {}
  };

  const formatTime = (s: number) => {
    const m = Math.floor(s / 60);
    const sec = s % 60;
    return `${m}:${sec.toString().padStart(2, "0")}`;
  };

  return (
    <View style={styles.container}>
      <Pressable style={styles.backBtn} onPress={onBack}>
        <Text style={styles.backText}>← Back</Text>
      </Pressable>

      <View style={styles.header}>
        <Text style={styles.patientName}>{visit.patient_name}</Text>
        <Text style={styles.clinicianName}>{visit.clinician_name}</Text>
      </View>

      {phase === "recording" && (
        <>
          <View style={styles.timerRow}>
            <View style={styles.recordingDot} />
            <Text style={styles.timer}>{formatTime(elapsed)}</Text>
          </View>
          <Text style={styles.hint}>Audio is being transcribed in real time</Text>
        </>
      )}

      {phase === "processing" && (
        <View style={styles.processingRow}>
          <ActivityIndicator color="#60A5FA" />
          <Text style={styles.processingText}>Generating SOAP note…</Text>
        </View>
      )}

      {phase === "done" && (
        <Text style={styles.doneText}>Note complete — opening…</Text>
      )}

      {phase === "error" && (
        <Text style={styles.errorText}>{error}</Text>
      )}

      <View style={styles.transcriptBox}>
        <Text style={styles.transcriptLabel}>Live Transcript</Text>
        <ScrollView style={styles.transcriptScroll}>
          <Text style={styles.transcriptText}>
            {transcriptSegments.join(" ") || "Listening…"}
          </Text>
          {noteStreamBuffer.length > 0 && (
            <>
              <Text style={[styles.transcriptLabel, { marginTop: 16 }]}>Generating Note…</Text>
              <Text style={styles.noteStream}>{noteStreamBuffer}</Text>
            </>
          )}
        </ScrollView>
      </View>

      {phase === "recording" && (
        <Pressable style={styles.endBtn} onPress={handleEndVisit}>
          <Text style={styles.endBtnText}>End Visit</Text>
        </Pressable>
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#0F172A", paddingHorizontal: 20, paddingTop: 60 },
  backBtn: { marginBottom: 16 },
  backText: { color: "#60A5FA", fontSize: 14 },
  header: { marginBottom: 24 },
  patientName: { fontSize: 22, fontWeight: "800", color: "#F8FAFC" },
  clinicianName: { fontSize: 13, color: "#64748B", marginTop: 2 },
  timerRow: { flexDirection: "row", alignItems: "center", gap: 10, marginBottom: 8 },
  recordingDot: {
    width: 10, height: 10, borderRadius: 5, backgroundColor: "#EF4444",
  },
  timer: { fontSize: 28, fontWeight: "700", color: "#F8FAFC", fontVariant: ["tabular-nums"] },
  hint: { fontSize: 12, color: "#475569", marginBottom: 20 },
  processingRow: { flexDirection: "row", alignItems: "center", gap: 12, marginBottom: 20 },
  processingText: { color: "#94A3B8", fontSize: 14 },
  doneText: { color: "#22C55E", fontSize: 14, marginBottom: 16 },
  errorText: { color: "#EF4444", fontSize: 13, marginBottom: 16 },
  transcriptBox: {
    flex: 1,
    backgroundColor: "#1E293B",
    borderRadius: 14,
    padding: 16,
    marginBottom: 20,
  },
  transcriptLabel: { fontSize: 11, fontWeight: "700", color: "#475569", marginBottom: 8, textTransform: "uppercase" },
  transcriptScroll: { flex: 1 },
  transcriptText: { fontSize: 14, color: "#CBD5E1", lineHeight: 22 },
  noteStream: { fontSize: 12, color: "#64748B", fontFamily: "monospace", lineHeight: 18 },
  endBtn: {
    backgroundColor: "#EF4444",
    borderRadius: 14,
    paddingVertical: 16,
    alignItems: "center",
    marginBottom: 32,
  },
  endBtnText: { color: "#fff", fontWeight: "700", fontSize: 16 },
});
