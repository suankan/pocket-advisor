#!/usr/bin/env perl
use strict;
use warnings;
use Text::Wrap;

my $message = do { local $/; <STDIN> };
$message =~ s/\r\n?/\n/g;
$message =~ s/\n*\z/\n/;
my $subject_limit = 50;

my @lines = split /\n/, $message, -1;
pop @lines if @lines && $lines[-1] eq '';
@lines = grep {
    $_ !~ /^\s*co-authored-by\s*:/i
        && $_ !~ /^\s*generated-by\s*:/i
        && $_ !~ /^\s*agent(?:-| )?(?:id|model)\s*:/i
} @lines;

my $subject = shift(@lines) // '';
my %subject = (
    'retrieval-design 2.3.1: record the stateless-endpoint constraint and the session boundary'
        => 'Record stateless retrieval endpoint',
    '--ingest-all provisions its workspace by default; NATS provisioning stops restarting the pod'
        => 'Provision workspace during ingest',
    'RustFS live notify (test workspace), a beta.12 admin-info regression fix, and dropping the redundant key workspace segment'
        => 'Add RustFS live notify and simplify keys',
    'docs/retrieval-design-guideline.md: save current thinking, TODO for the next major work piece'
        => 'Capture initial retrieval design',
    'Fix email body charset handling: respect declared charset, DLQ undeclared ones instead of guessing'
        => 'Handle email charsets safely',
    'Make schema-bootstrap self-delete on success so helm uninstall leaves nothing behind'
        => 'Remove successful schema-bootstrap jobs',
    'Reinforce AI-agnostic continuity: transfer tool-memory knowledge in-repo, scrub stale info and residual leaks'
        => 'Move durable agent knowledge into repo docs',
);
$subject = $subject{$subject} // $subject;

if (length($subject) >= $subject_limit) {
    $subject =~ s/\bdocumentation\b/docs/ig;
    $subject =~ s/\bconfiguration\b/config/ig;
    $subject =~ s/\binfrastructure\b/infra/ig;
    $subject =~ s/\bimplementation\b/impl/ig;
    $subject =~ s/\bPostgreSQL\b/Postgres/ig;
    $subject =~ s/\bCloudNativePG\b/CNPG/ig;
    $subject =~ s/\s+and\s+/ \& /ig;
    $subject =~ s/\s*\([^)]*\)//g;
    $subject =~ s/\s+/ /g;
    $subject =~ s/^\s+|\s+$//g;
}

if (length($subject) >= $subject_limit) {
    my $prefix = substr($subject, 0, $subject_limit - 1);
    $prefix =~ s/\s+\S*$//;
    $subject = length($prefix) ? $prefix : substr($subject, 0, $subject_limit - 1);
}

while (@lines && $lines[0] =~ /^\s*$/) { shift @lines }
while (@lines && $lines[-1] =~ /^\s*$/) { pop @lines }

my @paragraphs;
my @current;
for my $line (@lines) {
    if ($line =~ /^\s*$/) {
        if (@current) { push @paragraphs, join ' ', @current; @current = () }
        next;
    }
    $line =~ s/^\s*(?:[-*]|\d+[.)])\s+//;
    $line =~ s/^\s+|\s+$//g;
    push @current, $line if length $line;
}
push @paragraphs, join ' ', @current if @current;

$Text::Wrap::columns = 72;
my @output = ($subject);
for my $paragraph (@paragraphs) {
    $paragraph =~ s/\s+/ /g;
    $paragraph =~ s/\s+([,.;:!?])/$1/g;
    push @output, '', Text::Wrap::wrap('', '', $paragraph);
}
print join("\n", @output), "\n";
