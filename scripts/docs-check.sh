#!/usr/bin/env bash
# Fails if a relative link in a Markdown file points at a path that is missing
# or git-ignored (so it would be missing on GitHub, for example a local spec)
# or at a heading that does not exist. New files that are not yet staged count,
# so the gate passes before the commit that adds them. External links are not
# fetched, so the gate never depends on the network. Specs and plans under
# docs/superpowers/ are local working files: they are not scanned, and no
# link may point at them.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "$root"

exec perl -e '
use strict;
use warnings;

# Every path git tracks or would track (untracked but not ignored), and every
# directory that holds one.
my (%tracked, %dirs);
open(my $ls, "-|", "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard") or die "docs-check: cannot run git ls-files: $!\n";
{
    local $/ = "\0";
    while (my $path = <$ls>) {
        chomp $path;
        $tracked{$path} = 1;
        my $dir = $path;
        while ($dir =~ s{/[^/]+$}{}) { $dirs{$dir} = 1; }
    }
}
close($ls) or die "docs-check: git ls-files failed\n";
die "docs-check: git ls-files listed no files\n" unless %tracked;

my @docs = sort grep { /\.md$/ && !m{^docs/superpowers/} && -f $_ } keys %tracked;
die "docs-check: no Markdown files to check\n" unless @docs;

# GitHub heading anchors: lower case, punctuation dropped, spaces to hyphens,
# and a -1, -2 suffix for repeated headings.
my %anchors;
sub anchors_of {
    my ($file) = @_;
    return $anchors{$file} if $anchors{$file};
    open(my $fh, "<:encoding(UTF-8)", $file) or die "docs-check: cannot read $file: $!\n";
    my (%set, %seen, $fence);
    while (my $line = <$fh>) {
        if ($line =~ /^\s*(```|~~~)/) { $fence = !$fence; next; }
        next if $fence;
        next unless $line =~ /^#{1,6}\s+(.*?)\s*#*\s*$/;
        my $text = $1;
        $text =~ s/\[([^\]]*)\]\([^)]*\)/$1/g;
        $text =~ s/<[^>]+>//g;
        my $slug = lc $text;
        $slug =~ s/[^\w\- ]//g;
        $slug =~ s/ /-/g;
        my $n = $seen{$slug}++;
        $set{$n ? "$slug-$n" : $slug} = 1;
    }
    close($fh);
    return $anchors{$file} = \%set;
}

sub normalise {
    my ($path) = @_;
    my @out;
    for my $part (split m{/}, $path) {
        next if $part eq "" || $part eq ".";
        if ($part eq "..") {
            return undef unless @out;
            pop @out;
            next;
        }
        push @out, $part;
    }
    return join("/", @out);
}

my @errors;
for my $doc (@docs) {
    open(my $fh, "<:encoding(UTF-8)", $doc) or die "docs-check: cannot read $doc: $!\n";
    my $base = $doc =~ m{^(.*)/} ? $1 : "";
    my $fence;
    while (my $line = <$fh>) {
        if ($line =~ /^\s*(```|~~~)/) { $fence = !$fence; next; }
        next if $fence;
        (my $text = $line) =~ s/`[^`]*`//g;
        # Inline links and images, reference definitions, and HTML href/src.
        # A link split across lines is not seen.
        my @targets = $text =~ /\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)/g;
        push @targets, $1 if $text =~ /^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?/;
        push @targets, $text =~ /\b(?:href|src)="([^"]+)"/g;
        for my $target (@targets) {
            next if $target =~ m{^[a-z][a-z0-9+.-]*:}i || $target =~ m{^//};
            my ($path, $anchor) = split /#/, $target, 2;
            $path =~ s/%([0-9A-Fa-f]{2})/chr(hex($1))/ge;
            my $where = "$doc:$.";
            my $file;
            if ($path eq "") {
                $file = $doc;
            } else {
                my $joined = $path =~ m{^/} ? $path : ($base eq "" ? $path : "$base/$path");
                $file = normalise($joined);
                if (!defined $file) {
                    push @errors, "$where: $target leaves the repository";
                    next;
                }
                if ($file =~ m{^docs/superpowers(/|$)}) {
                    push @errors, "$where: $target is a local spec or plan; describe it in the README instead";
                    next;
                }
                if ((!$tracked{$file} && !$dirs{$file}) || !-e $file) {
                    push @errors, "$where: $target is missing or git-ignored";
                    next;
                }
            }
            next unless defined $anchor && $anchor ne "";
            next unless $file =~ /\.md$/ && $tracked{$file};
            $anchor =~ s/%([0-9A-Fa-f]{2})/chr(hex($1))/ge;
            push @errors, "$where: $target has no heading #$anchor"
                unless anchors_of($file)->{lc $anchor};
        }
    }
    close($fh);
}

if (@errors) {
    print STDERR "docs-check: broken links:\n";
    print STDERR "  $_\n" for @errors;
    exit 1;
}
printf "docs-check: %d Markdown files, links OK\n", scalar @docs;
'
