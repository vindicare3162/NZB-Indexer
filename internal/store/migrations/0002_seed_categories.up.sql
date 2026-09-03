-- Seed default Newznab categories (parents then children). IDs follow the
-- widely-used Newznab convention so downstream tools map categories correctly.
INSERT INTO categories (id, parent_id, name, description) VALUES
    (1000, NULL, 'Console', 'Console'),
    (1010, 1000, 'Console/NDS', 'Nintendo DS'),
    (1020, 1000, 'Console/PSP', 'Sony PSP'),
    (1030, 1000, 'Console/Wii', 'Nintendo Wii'),
    (1040, 1000, 'Console/XBOX', 'Microsoft Xbox'),
    (1050, 1000, 'Console/XBOX360', 'Microsoft Xbox 360'),
    (1080, 1000, 'Console/PS3', 'Sony PlayStation 3'),
    (1090, 1000, 'Console/Other', 'Other console platforms'),

    (2000, NULL, 'Movies', 'Movies'),
    (2010, 2000, 'Movies/Foreign', 'Foreign-language movies'),
    (2020, 2000, 'Movies/Other', 'Other movies'),
    (2030, 2000, 'Movies/SD', 'Standard-definition movies'),
    (2040, 2000, 'Movies/HD', 'High-definition movies'),
    (2045, 2000, 'Movies/UHD', 'Ultra-high-definition movies'),
    (2050, 2000, 'Movies/BluRay', 'BluRay movies'),
    (2060, 2000, 'Movies/3D', '3D movies'),

    (3000, NULL, 'Audio', 'Audio'),
    (3010, 3000, 'Audio/MP3', 'MP3 audio'),
    (3020, 3000, 'Audio/Video', 'Music videos'),
    (3030, 3000, 'Audio/Audiobook', 'Audiobooks'),
    (3040, 3000, 'Audio/Lossless', 'Lossless audio'),

    (4000, NULL, 'PC', 'PC'),
    (4010, 4000, 'PC/0day', 'PC 0-day'),
    (4020, 4000, 'PC/ISO', 'PC ISO'),
    (4030, 4000, 'PC/Mac', 'Mac software'),
    (4040, 4000, 'PC/Mobile-Other', 'Mobile - other'),
    (4050, 4000, 'PC/Games', 'PC games'),
    (4060, 4000, 'PC/Mobile-iOS', 'Mobile - iOS'),
    (4070, 4000, 'PC/Mobile-Android', 'Mobile - Android'),

    (5000, NULL, 'TV', 'TV'),
    (5020, 5000, 'TV/Foreign', 'Foreign-language TV'),
    (5030, 5000, 'TV/SD', 'Standard-definition TV'),
    (5040, 5000, 'TV/HD', 'High-definition TV'),
    (5045, 5000, 'TV/UHD', 'Ultra-high-definition TV'),
    (5050, 5000, 'TV/Other', 'Other TV'),
    (5060, 5000, 'TV/Sport', 'Sports'),
    (5070, 5000, 'TV/Anime', 'Anime'),
    (5080, 5000, 'TV/Documentary', 'Documentaries'),

    (6000, NULL, 'XXX', 'Adult'),
    (6010, 6000, 'XXX/DVD', 'Adult DVD'),
    (6020, 6000, 'XXX/WMV', 'Adult WMV'),
    (6030, 6000, 'XXX/XviD', 'Adult XviD'),
    (6040, 6000, 'XXX/x264', 'Adult x264'),
    (6060, 6000, 'XXX/Other', 'Adult other'),

    (7000, NULL, 'Books', 'Books'),
    (7010, 7000, 'Books/Mags', 'Magazines'),
    (7020, 7000, 'Books/Ebook', 'Ebooks'),
    (7030, 7000, 'Books/Comics', 'Comics'),
    (7040, 7000, 'Books/Technical', 'Technical books'),
    (7060, 7000, 'Books/Foreign', 'Foreign-language books'),

    (8000, NULL, 'Other', 'Other'),
    (8010, 8000, 'Other/Misc', 'Miscellaneous')
ON CONFLICT (id) DO NOTHING;
