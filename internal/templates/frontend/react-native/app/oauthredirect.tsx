// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// Route for the OAuth redirect: `<bundle id>:/oauthredirect` today, and
// `https://<domain>/oauthredirect` once you add a universal link — the path is
// the same, so this one file serves both.
//
// The system browser hands the URL back to the in-flight openAuthSessionAsync()
// call, and THAT is what finishes the sign-in. But the OS can also deliver the
// same URL to the app as a deep link (always on Android, sometimes on iOS), and
// without a matching route expo-router shows "Unmatched route". This screen
// absorbs it: it never reads the URL's code or state, and sends the user home,
// where the guard in app/_layout.tsx picks the sign-in screen or the app.
import { Redirect } from "expo-router";

export default function OAuthRedirectRoute() {
  return <Redirect href="/" />;
}
