# Plan review

Judge the plan against the checkout and cite evidence for each finding. Check:

(a) Work already on the default branch or in another active plan.
(b) Specs contradicting the code or docs/design.md.
(c) Tasks or plans touching the same files and risking conflicts. Flag fragmentation: adjacent
    tasks on the same file differing only by key or line ranges, and several same-type tasks
    reviewable as one coherent surface.
(d) Dependency shape and width, including unrelated work chained and dependent work parallel.
    Flag standalone changelog or evidence-correction tasks following every small fix, and
    acceptance scheduled while later mutation tasks are still planned (it should use
    `wait_landed` on them). Recommend a coherent work package with internal checkpoints and
    `continue` where useful; the goal is not giant tasks, since context limits and
    reviewability still apply, and independent acceptance stays a fresh session where the
    product requires it.
(e) Tier and effort that do not fit task difficulty.
(f) Missing or vague checks or acceptance criteria.
(g) Tasks too large for one job, judged against the task-size guide in foreman.md;
    suggest a chain for work well over the guide.
(h) Open each named skill's SKILL.md, judge fit against the spec and cite the
    description line matched. Flag misfits and an obvious skill not picked; respect
    the user's choices.
