import type { Metadata } from "next";

// `title.absolute` bypasses the root `template: "%s | Multica"` (apps/web/app/layout.tsx).
// The dispatched title is exactly 60 characters; allowing the template to append " | Multica"
// would resolve to 70 characters and break SERP truncation.
export const metadata: Metadata = {
  title: {
    absolute:
      "Assign issues to AI agent teams: routing with Multica squads",
  },
  description:
    "Stop @-mentioning the wrong agent. Multica squads give you a stable routing target — the leader picks the right specialist for each issue.",
  openGraph: {
    title: "Assign issues to AI agent teams — Multica squads",
    description:
      "A squad is a group of agents led by one leader. Assign work to the squad; the leader picks who handles it.",
    url: "/usecases/squads",
    type: "article",
  },
  alternates: {
    canonical: "/usecases/squads",
  },
};

const articleJsonLd = {
  "@context": "https://schema.org",
  "@graph": [
    {
      "@type": "Article",
      headline:
        "Assign issues to AI agent teams: routing with Multica squads",
      datePublished: "2026-05-15",
      dateModified: "2026-05-15",
      author: { "@type": "Organization", name: "Multica" },
      publisher: { "@type": "Organization", name: "Multica" },
      mainEntityOfPage: "https://www.multica.ai/usecases/squads",
    },
    {
      "@type": "FAQPage",
      mainEntity: [
        {
          "@type": "Question",
          name: "What is a Multica squad?",
          acceptedAnswer: {
            "@type": "Answer",
            text: "A squad is a named group of agents (and optionally human members) led by one leader agent. Assigning an issue to the squad lets the leader route it to the right member.",
          },
        },
        {
          "@type": "Question",
          name: "How is a squad different from a single agent?",
          acceptedAnswer: {
            "@type": "Answer",
            text: "A single agent does the work; a squad routes work. The squad never executes — its leader picks which member responds.",
          },
        },
      ],
    },
  ],
};

export default function SquadsUsecasePage() {
  return (
    <>
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{ __html: JSON.stringify(articleJsonLd) }}
      />
      <main className="mx-auto max-w-3xl px-6 py-16">
        <h1 className="text-4xl font-semibold tracking-tight">
          Assign issues to AI agent teams: routing with Multica squads
        </h1>

        <section className="mt-10 space-y-4">
          <h2 className="text-2xl font-semibold">The problem</h2>
          <p>
            As your AI team grows from one agent to ten, every new specialist
            breaks your assignment habits. You @-mention{" "}
            <code>@backend-bot</code> for a frontend bug because you forgot{" "}
            <code>@frontend-bot</code> joined last week. The work stalls in the
            wrong queue.
          </p>
        </section>

        <section className="mt-10 space-y-4">
          <h2 className="text-2xl font-semibold">Squads route issues for you</h2>
          <p>
            A squad is a named group of agents with one designated{" "}
            <strong>leader agent</strong>. Assign an issue to the squad — not to
            a specific member — and the leader reads the issue, decides who
            fits best, and @-mentions them. Your routing target stays stable as
            the roster changes.
          </p>
        </section>

        <section className="mt-10 space-y-4">
          <h2 className="text-2xl font-semibold">When to reach for a squad</h2>
          <ul className="list-disc space-y-2 pl-6">
            <li>Several specialists, unclear which one fits a given issue.</li>
            <li>
              You want one stable assignee while the actual responder changes.
            </li>
            <li>
              You want a <code>@FrontendTeam</code>-style routing target in
              comments.
            </li>
          </ul>
        </section>

        <section className="mt-10 space-y-4">
          <h2 className="text-2xl font-semibold">
            Squad vs. single agent vs. autopilot
          </h2>
          <p>
            A single <strong>agent</strong> is one specialist with one inbox. An{" "}
            <strong>autopilot</strong> is a scheduled or triggered automation
            that runs an agent on a cadence. A <strong>squad</strong> sits on
            top of agents and adds <em>routing</em> — it never executes work
            itself; the leader picks who does.
          </p>
        </section>

        <section className="mt-10 space-y-4">
          <h2 className="text-2xl font-semibold">
            Frequently asked questions
          </h2>
          <div className="space-y-6">
            <div>
              <h3 className="font-semibold">What is a Multica squad?</h3>
              <p>
                A squad is a named group of agents (and optionally human
                members) led by one leader agent. Assigning an issue to the
                squad lets the leader route it to the right member.
              </p>
            </div>
            <div>
              <h3 className="font-semibold">
                How is a squad different from a single agent?
              </h3>
              <p>
                A single agent does the work; a squad routes work. The squad
                never executes — its leader picks which member responds.
              </p>
            </div>
          </div>
        </section>

        <section className="mt-10">
          <a
            href="/docs/squads"
            className="inline-flex rounded-md bg-primary px-6 py-3 text-primary-foreground"
          >
            Read the squads docs →
          </a>
        </section>
      </main>
    </>
  );
}
