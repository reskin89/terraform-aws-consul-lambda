container {
	dependencies = true
	alpine_secdb = true
	secrets {
		all = true
		skip_path_strings = ["/usr/lib64/libunistring.so.0.1.2"]
	}
}

binary {
	secrets      = true
	go_modules   = true
	osv          = true
	oss_index    = false
	nvd          = false
}
