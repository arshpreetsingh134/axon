package official

// ConformsTo is the version of the reference pipeline tool that axon's official
// checks were verified against (SPEC.md Appendix C; plan decision D3). When a
// cluster's workflow file pins a different version, axon warns but does not
// block.
const ConformsTo = "0.1.80"
