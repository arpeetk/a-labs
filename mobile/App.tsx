import { StatusBar } from "expo-status-bar";
import React, { useState } from "react";
import { SafeAreaView, StyleSheet } from "react-native";
import { Visit } from "./src/api";
import HomeScreen from "./src/screens/HomeScreen";
import NoteScreen from "./src/screens/NoteScreen";
import RecordingScreen from "./src/screens/RecordingScreen";

type Screen =
  | { name: "home" }
  | { name: "recording"; visit: Visit }
  | { name: "note"; visitId: number };

export default function App() {
  const [screen, setScreen] = useState<Screen>({ name: "home" });

  return (
    <SafeAreaView style={styles.root}>
      <StatusBar style="light" />

      {screen.name === "home" && (
        <HomeScreen
          onStartVisit={(visit) => setScreen({ name: "recording", visit })}
          onOpenVisit={(visit) => setScreen({ name: "note", visitId: visit.id })}
        />
      )}

      {screen.name === "recording" && (
        <RecordingScreen
          visit={screen.visit}
          onComplete={(visitId) => setScreen({ name: "note", visitId })}
          onBack={() => setScreen({ name: "home" })}
        />
      )}

      {screen.name === "note" && (
        <NoteScreen
          visitId={screen.visitId}
          onBack={() => setScreen({ name: "home" })}
        />
      )}
    </SafeAreaView>
  );
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: "#0F172A" },
});
