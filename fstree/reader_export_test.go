package fstree

// Held reports how many decoded directory objects and how many commit-to-tree
// answers r holds, for the tests of its bounds.
func (r *DirectoryReader) Held() (directories, commits int) {
	return len(r.directories), len(r.commits)
}
