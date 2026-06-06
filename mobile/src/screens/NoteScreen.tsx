import React, { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  View,
} from "react-native";
import { SoapNote, Visit, getVisit } from "../api";

interface Props {
  visitId: number;
  onBack: () => void;
}

interface EvidencePillProps {
  evidence: string | null;
}

function EvidencePill({ evidence }: EvidencePillProps) {
  const [expanded, setExpanded] = useState(false);
  if (!evidence) return null;
  return (
    <Pressable style={styles.evidencePill} onPress={() => setExpanded((e) => !e)}>
      <Text style={styles.evidenceIcon}>🔗</Text>
      {expanded ? (
        <Text style={styles.evidenceText}>"{evidence}"</Text>
      ) : (
        <Text style={styles.evidenceTextCollapsed}>View source transcript</Text>
      )}
    </Pressable>
  );
}

interface SectionProps {
  label: string;
  section: { text: string | null; evidence: string | null } | null;
}

function NoteSection({ label, section }: SectionProps) {
  if (!section?.text) return null;
  return (
    <View style={styles.noteSection}>
      <Text style={styles.noteSectionLabel}>{label}</Text>
      <Text style={styles.noteSectionText}>{section.text}</Text>
      <EvidencePill evidence={section.evidence} />
    </View>
  );
}

function HallucinationBadge({ level }: { level: SoapNote["hallucination_check"] }) {
  const colors: Record<string, string> = {
    none: "#22C55E",
    low: "#84CC16",
    medium: "#F59E0B",
    high: "#EF4444",
  };
  const color = colors[level] ?? "#64748B";
  return (
    <View style={[styles.hallucinationBadge, { borderColor: color }]}>
      <Text style={[styles.hallucinationText, { color }]}>
        Confabulation risk: {level}
      </Text>
    </View>
  );
}

export default function NoteScreen({ visitId, onBack }: Props) {
  const [visit, setVisit] = useState<Visit | null>(null);
  const [loading, setLoading] = useState(true);

  const loadVisit = useCallback(async () => {
    try {
      const v = await getVisit(visitId);
      setVisit(v);
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  }, [visitId]);

  useEffect(() => {
    loadVisit();
  }, [loadVisit]);

  if (loading) {
    return (
      <View style={styles.centered}>
        <ActivityIndicator color="#60A5FA" size="large" />
      </View>
    );
  }

  const note = visit?.note as SoapNote | null;

  return (
    <View style={styles.container}>
      <View style={styles.topBar}>
        <Pressable onPress={onBack}>
          <Text style={styles.backText}>← Back</Text>
        </Pressable>
        <Text style={styles.topBarTitle}>SOAP Note</Text>
        <View style={{ width: 48 }} />
      </View>

      <ScrollView style={styles.scroll} contentContainerStyle={styles.scrollContent}>
        {visit && (
          <View style={styles.visitMeta}>
            <Text style={styles.metaPatient}>{visit.patient_name}</Text>
            <Text style={styles.metaClinician}>
              {visit.clinician_name} · {new Date(visit.created_at).toLocaleString()}
            </Text>
            {note?.hallucination_check && (
              <HallucinationBadge level={note.hallucination_check} />
            )}
          </View>
        )}

        {!note ? (
          <View style={styles.emptyNote}>
            <Text style={styles.emptyNoteText}>
              {visit?.status === "generating"
                ? "Generating note…"
                : "No note available for this visit."}
            </Text>
            {visit?.status === "generating" && <ActivityIndicator color="#60A5FA" style={{ marginTop: 16 }} />}
          </View>
        ) : (
          <>
            <Text style={styles.soapHeader}>S — Subjective</Text>
            <NoteSection label="Chief Complaint" section={note.subjective?.chief_complaint} />
            <NoteSection label="History of Present Illness" section={note.subjective?.history_of_present_illness} />
            <NoteSection label="Review of Systems" section={note.subjective?.review_of_systems} />

            <Text style={styles.soapHeader}>O — Objective</Text>
            <NoteSection label="Vitals" section={note.objective?.vitals} />
            <NoteSection label="Physical Exam" section={note.objective?.physical_exam} />

            <Text style={styles.soapHeader}>A — Assessment</Text>
            <NoteSection label="Diagnosis" section={note.assessment?.diagnosis} />
            <NoteSection label="Differential" section={note.assessment?.differential} />

            <Text style={styles.soapHeader}>P — Plan</Text>
            <NoteSection label="Treatment" section={note.plan?.treatment} />
            <NoteSection label="Follow-up" section={note.plan?.follow_up} />
            <NoteSection label="Patient Instructions" section={note.plan?.patient_instructions} />

            {visit?.transcript && (
              <>
                <Text style={[styles.soapHeader, { marginTop: 32 }]}>Full Transcript</Text>
                <View style={styles.transcriptBox}>
                  <Text style={styles.transcriptText}>{visit.transcript}</Text>
                </View>
              </>
            )}
          </>
        )}
      </ScrollView>
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#0F172A" },
  centered: { flex: 1, backgroundColor: "#0F172A", justifyContent: "center", alignItems: "center" },
  topBar: {
    flexDirection: "row",
    alignItems: "center",
    justifyContent: "space-between",
    paddingHorizontal: 20,
    paddingTop: 60,
    paddingBottom: 16,
    borderBottomWidth: 1,
    borderBottomColor: "#1E293B",
  },
  backText: { color: "#60A5FA", fontSize: 14, width: 48 },
  topBarTitle: { fontSize: 16, fontWeight: "700", color: "#F8FAFC" },
  scroll: { flex: 1 },
  scrollContent: { padding: 20, paddingBottom: 60 },
  visitMeta: { marginBottom: 24 },
  metaPatient: { fontSize: 20, fontWeight: "800", color: "#F8FAFC" },
  metaClinician: { fontSize: 12, color: "#64748B", marginTop: 2 },
  hallucinationBadge: {
    marginTop: 10,
    borderWidth: 1,
    borderRadius: 20,
    paddingHorizontal: 12,
    paddingVertical: 4,
    alignSelf: "flex-start",
  },
  hallucinationText: { fontSize: 11, fontWeight: "700" },
  soapHeader: {
    fontSize: 11,
    fontWeight: "800",
    color: "#3B82F6",
    textTransform: "uppercase",
    letterSpacing: 1,
    marginTop: 24,
    marginBottom: 8,
  },
  noteSection: {
    backgroundColor: "#1E293B",
    borderRadius: 12,
    padding: 14,
    marginBottom: 8,
  },
  noteSectionLabel: { fontSize: 11, color: "#475569", fontWeight: "600", marginBottom: 4 },
  noteSectionText: { fontSize: 14, color: "#CBD5E1", lineHeight: 20 },
  evidencePill: {
    flexDirection: "row",
    alignItems: "flex-start",
    marginTop: 10,
    backgroundColor: "#0F172A",
    borderRadius: 8,
    padding: 8,
    gap: 6,
  },
  evidenceIcon: { fontSize: 12 },
  evidenceText: { fontSize: 12, color: "#60A5FA", lineHeight: 18, flex: 1, fontStyle: "italic" },
  evidenceTextCollapsed: { fontSize: 12, color: "#475569" },
  emptyNote: { alignItems: "center", marginTop: 60 },
  emptyNoteText: { color: "#475569", fontSize: 14 },
  transcriptBox: { backgroundColor: "#1E293B", borderRadius: 12, padding: 14 },
  transcriptText: { fontSize: 13, color: "#64748B", lineHeight: 20 },
});
