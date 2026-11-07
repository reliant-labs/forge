// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// The sign-in screen. Deliberately plain: replace it with your product's own.
// It exists to prove the loop end to end (system browser → callback → tokens
// in secure storage → signed-in screens), and shows the states that loop has:
// working, cancelled, offline, storage unavailable, another browser open, and
// a misconfigured build. app/_layout.tsx shows it only when the app is
// configured for native sign-in and nobody is signed in.
import { useCallback, useState } from "react";
import { ActivityIndicator, Pressable, StyleSheet, Text, View } from "react-native";

import { useAuth } from "../src/lib/auth/use-auth";
import { describeSignInError } from "../src/lib/auth/sign-in-error";

type Status =
  | { state: "idle" }
  | { state: "working" }
  | { state: "cancelled" }
  | { state: "error"; message: string; retryable: boolean };

export default function SignInScreen() {
  const { signIn, configError } = useAuth();
  const [status, setStatus] = useState<Status>({ state: "idle" });

  const onPress = useCallback(async () => {
    setStatus({ state: "working" });
    try {
      const user = await signIn();
      // Signed in: the layout's guard moves on by itself. Cancelled: stay here.
      setStatus(user ? { state: "idle" } : { state: "cancelled" });
    } catch (e) {
      console.warn("[auth] sign-in failed:", e);
      const info = describeSignInError(e);
      setStatus({ state: "error", message: info.message, retryable: info.retryable });
    }
  }, [signIn]);

  const working = status.state === "working";
  const blocked = configError !== null || (status.state === "error" && !status.retryable);

  return (
    <View style={styles.container}>
      <Text style={styles.title}>Sign in</Text>
      {configError ? (
        <Text accessibilityRole="alert" style={styles.error}>
          {describeSignInError(configError).message}
        </Text>
      ) : null}
      {status.state === "cancelled" ? (
        <Text style={styles.note}>Sign-in was cancelled.</Text>
      ) : null}
      {status.state === "error" ? (
        <Text accessibilityRole="alert" style={styles.error}>
          {status.message}
        </Text>
      ) : null}
      <Pressable
        accessibilityRole="button"
        accessibilityState={{ disabled: working || blocked, busy: working }}
        disabled={working || blocked}
        onPress={onPress}
        style={[styles.button, (working || blocked) && styles.buttonDisabled]}
      >
        {working ? (
          <ActivityIndicator color="#fff" />
        ) : (
          <Text style={styles.buttonText}>Sign in</Text>
        )}
      </Pressable>
    </View>
  );
}

const styles = StyleSheet.create({
  container: { flex: 1, alignItems: "center", justifyContent: "center", padding: 24, gap: 16 },
  title: { fontSize: 28, fontWeight: "bold" },
  note: { fontSize: 14, color: "#666", textAlign: "center" },
  error: { fontSize: 14, color: "#b00020", textAlign: "center" },
  button: {
    minWidth: 200,
    minHeight: 48,
    alignItems: "center",
    justifyContent: "center",
    borderRadius: 8,
    backgroundColor: "#111",
    paddingHorizontal: 24,
  },
  buttonDisabled: { opacity: 0.5 },
  buttonText: { color: "#fff", fontSize: 16, fontWeight: "600" },
});
