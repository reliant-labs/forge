// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// Route for the post-logout redirect: `<bundle id>:/signedout` (and
// `https://<domain>/signedout` for a universal link). Same reason as
// ./oauthredirect: the OS may deliver the URL as a deep link after the IdP's
// end-session page, and it must not land on "Unmatched route".
import { Redirect } from "expo-router";

export default function SignedOutRoute() {
  return <Redirect href="/" />;
}
