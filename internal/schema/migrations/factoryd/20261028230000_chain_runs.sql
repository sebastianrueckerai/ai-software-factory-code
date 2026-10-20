-- the CI chain a change's merge runs on main (roadmap 297): the card's
-- deploy fact, read from the chain itself, for any project. One row per
-- pull request whose change a main run carries; the row's status is its
-- commit's verdict — the AND of the commit's runs and required contexts,
-- the value the head row writes — so a card is live when the chain that
-- carried its change went green, whatever the chain's tail is for the
-- project. A run no pull
-- request rides is recorded under 0, so the recorded head advances and no
-- card reads it.
CREATE TABLE IF NOT EXISTS chain_runs (
    repository text NOT NULL,
    pr_number bigint NOT NULL,
    sha text NOT NULL,
    run_id bigint NOT NULL,
    status text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository, pr_number)
);
