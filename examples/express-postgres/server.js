// A tiny Express + Postgres app: a visit counter.
// The schema is created by `node migrate.js`, which forge runs as a job
// before every rollout.
const express = require("express");
const { Pool } = require("pg");

const pool = new Pool({
  connectionString:
    process.env.DATABASE_URL ||
    "postgresql://postgres:postgres@localhost:5432/visits",
});

const app = express();

app.get("/healthz", async (_req, res) => {
  await pool.query("SELECT 1");
  res.json({ ok: true });
});

app.get("/", async (_req, res) => {
  const { rows } = await pool.query(
    "INSERT INTO visits DEFAULT VALUES RETURNING id",
  );
  res.json({ message: "hello from express", visit: Number(rows[0].id) });
});

const port = Number(process.env.PORT || 3000);
app.listen(port, () => console.log(`listening on :${port}`));
