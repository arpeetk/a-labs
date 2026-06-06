import React, { useCallback, useEffect, useState } from "react";
import {
  ActivityIndicator,
  FlatList,
  Pressable,
  StyleSheet,
  Text,
  TextInput,
  View,
} from "react-native";
import { Visit, createVisit, listVisits } from "../api";

interface Props {
  onStartVisit: (visit: Visit) => void;
  onOpenVisit: (visit: Visit) => void;
}

export default function HomeScreen({ onStartVisit, onOpenVisit }: Props) {
  const [patientName, setPatientName] = useState("");
  const [clinicianName, setClinicianName] = useState("");
  const [visits, setVisits] = useState<Visit[]>([]);
  const [loading, setLoading] = useState(false);
  const [creating, setCreating] = useState(false);

  const loadVisits = useCallback(async () => {
    setLoading(true);
    try {
      setVisits(await listVisits());
    } catch {
      // backend may not be reachable yet
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    loadVisits();
  }, [loadVisits]);

  const handleStart = async () => {
    if (!patientName.trim() || !clinicianName.trim()) return;
    setCreating(true);
    try {
      const visit = await createVisit(patientName.trim(), clinicianName.trim());
      setPatientName("");
      setClinicianName("");
      onStartVisit(visit);
    } catch (e: any) {
      console.error(e);
    } finally {
      setCreating(false);
    }
  };

  const statusColor = (status: Visit["status"]) => {
    if (status === "complete") return "#22C55E";
    if (status === "error") return "#EF4444";
    if (status === "recording") return "#F59E0B";
    return "#60A5FA";
  };

  return (
    <View style={styles.container}>
      <Text style={styles.title}>a-labs</Text>
      <Text style={styles.subtitle}>Clinical Documentation</Text>

      <View style={styles.card}>
        <Text style={styles.label}>Patient name</Text>
        <TextInput
          style={styles.input}
          placeholder="e.g. Jane Smith"
          placeholderTextColor="#64748B"
          value={patientName}
          onChangeText={setPatientName}
        />
        <Text style={styles.label}>Clinician name</Text>
        <TextInput
          style={styles.input}
          placeholder="e.g. Dr. Williams"
          placeholderTextColor="#64748B"
          value={clinicianName}
          onChangeText={setClinicianName}
        />
        <Pressable
          style={[styles.startBtn, (!patientName || !clinicianName) && styles.btnDisabled]}
          onPress={handleStart}
          disabled={creating || !patientName || !clinicianName}
        >
          {creating ? (
            <ActivityIndicator color="#fff" />
          ) : (
            <Text style={styles.startBtnText}>Start Visit</Text>
          )}
        </Pressable>
      </View>

      <View style={styles.historyHeader}>
        <Text style={styles.sectionTitle}>Recent Visits</Text>
        <Pressable onPress={loadVisits}>
          <Text style={styles.refreshBtn}>Refresh</Text>
        </Pressable>
      </View>

      {loading ? (
        <ActivityIndicator color="#60A5FA" style={{ marginTop: 24 }} />
      ) : (
        <FlatList
          data={visits}
          keyExtractor={(v) => String(v.id)}
          renderItem={({ item }) => (
            <Pressable style={styles.visitRow} onPress={() => onOpenVisit(item)}>
              <View style={styles.visitInfo}>
                <Text style={styles.visitPatient}>{item.patient_name}</Text>
                <Text style={styles.visitMeta}>
                  {item.clinician_name} · {new Date(item.created_at).toLocaleDateString()}
                </Text>
              </View>
              <View style={[styles.badge, { backgroundColor: statusColor(item.status) + "22" }]}>
                <Text style={[styles.badgeText, { color: statusColor(item.status) }]}>
                  {item.status}
                </Text>
              </View>
            </Pressable>
          )}
          ListEmptyComponent={
            <Text style={styles.emptyText}>No visits yet — start one above.</Text>
          }
        />
      )}
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, backgroundColor: "#0F172A", paddingHorizontal: 20, paddingTop: 60 },
  title: { fontSize: 28, fontWeight: "800", color: "#F8FAFC", letterSpacing: -0.5 },
  subtitle: { fontSize: 14, color: "#64748B", marginBottom: 28 },
  card: {
    backgroundColor: "#1E293B",
    borderRadius: 16,
    padding: 20,
    marginBottom: 32,
  },
  label: { fontSize: 12, color: "#94A3B8", fontWeight: "600", marginBottom: 6, marginTop: 12 },
  input: {
    backgroundColor: "#0F172A",
    borderRadius: 8,
    padding: 12,
    color: "#F8FAFC",
    fontSize: 15,
    borderWidth: 1,
    borderColor: "#334155",
  },
  startBtn: {
    marginTop: 20,
    backgroundColor: "#3B82F6",
    borderRadius: 10,
    paddingVertical: 14,
    alignItems: "center",
  },
  btnDisabled: { opacity: 0.4 },
  startBtnText: { color: "#fff", fontWeight: "700", fontSize: 15 },
  historyHeader: { flexDirection: "row", justifyContent: "space-between", alignItems: "center", marginBottom: 12 },
  sectionTitle: { fontSize: 16, fontWeight: "700", color: "#F8FAFC" },
  refreshBtn: { fontSize: 13, color: "#60A5FA" },
  visitRow: {
    flexDirection: "row",
    alignItems: "center",
    backgroundColor: "#1E293B",
    borderRadius: 12,
    padding: 16,
    marginBottom: 8,
  },
  visitInfo: { flex: 1 },
  visitPatient: { fontSize: 15, fontWeight: "600", color: "#F8FAFC" },
  visitMeta: { fontSize: 12, color: "#64748B", marginTop: 2 },
  badge: { paddingHorizontal: 10, paddingVertical: 4, borderRadius: 20 },
  badgeText: { fontSize: 11, fontWeight: "700" },
  emptyText: { color: "#475569", textAlign: "center", marginTop: 32 },
});
