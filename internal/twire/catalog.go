package twire

// catalog is the built-in set of fake well-known services twire can
// impersonate: common databases, Windows/AD services, remote-access
// protocols, and other frequently-scanned ports. Profiles are referenced
// by their stable Key (persisted in twire_canaries and twire_events);
// reordering or relabeling is safe, but changing a Key orphans the
// enabled-state and event rows tied to the old one.
//
// Banners are only set for protocols where the server greets first, so the
// canary looks alive to a banner grab; for client-speaks-first protocols
// the field is left empty.
var catalog = []ServiceProfile{
	{Key: "ftp", Name: "FTP", Port: 21, Description: "File Transfer Protocol", Banner: "220 (vsFTPd 3.0.5)\r\n"},
	{Key: "ssh", Name: "SSH", Port: 22, Description: "Secure Shell", Banner: "SSH-2.0-OpenSSH_8.9p1 Ubuntu-3ubuntu0.4\r\n"},
	{Key: "telnet", Name: "Telnet", Port: 23, Description: "Telnet remote login", Banner: "\r\nUbuntu 22.04.3 LTS\r\nlogin: "},
	{Key: "smtp", Name: "SMTP", Port: 25, Description: "Mail transfer", Banner: "220 mail.local ESMTP Postfix (Ubuntu)\r\n"},
	{Key: "dns", Name: "DNS", Port: 53, Description: "Domain Name System (TCP)"},
	{Key: "http", Name: "HTTP", Port: 80, Description: "Web server"},
	{Key: "pop3", Name: "POP3", Port: 110, Description: "Mail retrieval", Banner: "+OK POP3 ready\r\n"},
	{Key: "msrpc", Name: "MS-RPC", Port: 135, Description: "Microsoft RPC endpoint mapper (Windows)"},
	{Key: "netbios", Name: "NetBIOS", Port: 139, Description: "NetBIOS session service (Windows)"},
	{Key: "imap", Name: "IMAP", Port: 143, Description: "Mail retrieval", Banner: "* OK [CAPABILITY IMAP4rev1] Dovecot ready.\r\n"},
	{Key: "ldap", Name: "LDAP", Port: 389, Description: "Directory / Active Directory (Windows)"},
	{Key: "https", Name: "HTTPS", Port: 443, Description: "Web server (TLS)"},
	{Key: "smb", Name: "SMB", Port: 445, Description: "Windows file sharing (microsoft-ds)"},
	{Key: "mssql", Name: "Microsoft SQL Server", Port: 1433, Description: "MSSQL database (Windows)"},
	{Key: "oracle", Name: "Oracle DB", Port: 1521, Description: "Oracle database listener"},
	{Key: "docker", Name: "Docker API", Port: 2375, Description: "Unauthenticated Docker daemon API"},
	{Key: "mysql", Name: "MySQL", Port: 3306, Description: "MySQL / MariaDB database"},
	{Key: "rdp", Name: "RDP", Port: 3389, Description: "Windows Remote Desktop"},
	{Key: "postgresql", Name: "PostgreSQL", Port: 5432, Description: "PostgreSQL database"},
	{Key: "vnc", Name: "VNC", Port: 5900, Description: "Remote desktop (RFB)", Banner: "RFB 003.008\n"},
	{Key: "winrm", Name: "WinRM", Port: 5985, Description: "Windows Remote Management"},
	{Key: "redis", Name: "Redis", Port: 6379, Description: "Redis key-value store"},
	{Key: "elasticsearch", Name: "Elasticsearch", Port: 9200, Description: "Elasticsearch HTTP API"},
	{Key: "memcached", Name: "Memcached", Port: 11211, Description: "Memcached cache"},
	{Key: "mongodb", Name: "MongoDB", Port: 27017, Description: "MongoDB database"},
}

// catalogByKey indexes the catalog for O(1) lookup by profile key.
func catalogByKey() map[string]ServiceProfile {
	m := make(map[string]ServiceProfile, len(catalog))
	for _, p := range catalog {
		m[p.Key] = p
	}
	return m
}
