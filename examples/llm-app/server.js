// A tiny HTTP service that asks an LLM for a haiku.
//
// It holds no provider key. forge.llm_env() (deploy/kcl/workloads.k) hands it
// OPENAI_BASE_URL and OPENAI_API_KEY for Reliant's LLM gateway, and the stock
// OpenAI SDK reads both with no configuration here. When they are blank (an
// unclaimed claimable deploy, which has no outbound internet) it answers 503
// instead of crashing.
const http = require("node:http");
const OpenAI = require("openai");

const model = process.env.LLM_MODEL || "claude-haiku-4-5";
const client = process.env.OPENAI_API_KEY ? new OpenAI() : null;

async function haiku(topic) {
  const completion = await client.chat.completions.create({
    model,
    max_tokens: 100,
    messages: [{ role: "user", content: `Write a haiku about ${topic}.` }],
  });
  return completion.choices[0].message.content;
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://localhost");
  const send = (status, body) => {
    res.writeHead(status, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  };
  if (url.pathname === "/healthz") return send(200, { ok: true });
  if (url.pathname !== "/") return send(404, { error: "not found" });
  if (!client) {
    return send(503, {
      error:
        "LLM unavailable: no gateway key. Claim the deploy (or sign in and deploy without --claimable).",
    });
  }
  try {
    const topic = (
      url.searchParams.get("topic") || "deploying on a Friday"
    ).slice(0, 100);
    send(200, { model, topic, haiku: await haiku(topic) });
  } catch (err) {
    console.error(err);
    send(502, { error: `LLM call failed: ${err.message}` });
  }
});

const port = Number(process.env.PORT || 3000);
server.listen(port, () =>
  console.log(`listening on :${port} (model ${model})`),
);
